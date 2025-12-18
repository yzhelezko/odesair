package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
)

const (
	maxMessageHistory = 20
	systemMessageFile = "config/system_message.txt"
)

var sendToChannel = getEnv("SEND_TO_CHANNEL", "odesair")

func loadConfig() Config {
	appID, _ := strconv.Atoi(getEnv("APPID", ""))
	aiBatchIntervalStr := getEnv("AI_INTERACTION_INTERVAL", "30s")
	aiBatchInterval, err := time.ParseDuration(aiBatchIntervalStr)
	if err != nil {
		log.Printf("Invalid AI_INTERACTION_INTERVAL duration '%s', using default 30s: %v", aiBatchIntervalStr, err)
		aiBatchInterval = 30 * time.Second
	}

	aiBatchExtendDurationStr := getEnv("AI_BATCH_EXTEND_DURATION", "3s")
	aiBatchExtendDuration, err := time.ParseDuration(aiBatchExtendDurationStr)
	if err != nil {
		log.Printf("Invalid AI_BATCH_EXTEND_DURATION duration '%s', using default 3s: %v", aiBatchExtendDurationStr, err)
		aiBatchExtendDuration = 3 * time.Second
	}

	return Config{
		APIID:       appID,
		APIHash:     getEnv("APPHASH", ""),
		PhoneNumber: getEnv("PHONE_NUMBER", ""),
		Channels: []ChannelInfo{
			{Identifier: "odessa_infonews", IsPrivate: false},
			{Identifier: "xydessa_live", IsPrivate: false},
			{Identifier: "freechat_odesa", IsPrivate: false},
			{Identifier: "odesairxydessa", IsPrivate: false},
		},
		MessageLimit:          1,
		SessionFilePath:       "config/tdlib-session",
		UpdateInterval:        5 * time.Second,
		AIChoice:              getEnv("AI_CHOICE", "chatgpt"),
		AIAPIKey:              getEnv("API_KEY", ""),
		EnableTelegramSend:    getEnv("ENABLE_TELEGRAM_SEND", "true") == "true",
		IgnoreAirAttack:       getEnv("IGNORE_AIR_ATTACK", "false") == "true",
		AIBatchInterval:       aiBatchInterval,
		AIBatchExtendDuration: aiBatchExtendDuration,
	}
}

type Config struct {
	APIID                 int
	APIHash               string
	PhoneNumber           string
	Channels              []ChannelInfo
	MessageLimit          int
	SessionFilePath       string
	UpdateInterval        time.Duration
	AIChoice              string
	AIAPIKey              string
	EnableTelegramSend    bool
	IgnoreAirAttack       bool
	AIBatchInterval       time.Duration
	AIBatchExtendDuration time.Duration
}

type ChannelInfo struct {
	Identifier string
	IsPrivate  bool
}

type AIClient interface {
	SendMessage(ctx context.Context, message Message) (AIJSONResponse, error)
	AddMessageToHistory(message Message)
	GetMessageHistory() []Message
}

type AIJSONResponse struct {
	Text          string `json:"text" yaml:"text"`
	Principle     string `json:"principle" yaml:"principle"`
	Danger        bool   `json:"danger" yaml:"danger"`
	StatusChanged bool   `json:"statusChanged" yaml:"statusChanged"`
}

type Image struct {
	Data     []byte
	MIMEType string
}

type Message struct {
	Role    string  `json:"role"`
	Content string  `json:"content"`
	Images  []Image `json:"-"`
}

type ClaudeClient struct {
	APIKey         string
	HTTPClient     *http.Client
	SystemMessage  string
	MessageHistory []Message
}

type ChatGPTClient struct {
	APIKey         string
	HTTPClient     *http.Client
	SystemMessage  string
	MessageHistory []Message
}

type OpenRouterClient struct {
	APIKey         string
	HTTPClient     *http.Client
	SystemMessage  string
	MessageHistory []Message
}

type GeminiClient struct {
	APIKey         string
	HTTPClient     *http.Client
	SystemMessage  string
	MessageHistory []Message
}

func main() {
	config := loadConfig()

	log.Printf("Configuration:")
	log.Printf("  AI Choice: %s", config.AIChoice)
	log.Printf("  Ignore Air Attack: %v", config.IgnoreAirAttack)
	log.Printf("  Enable Telegram Send: %v", config.EnableTelegramSend)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	client := telegram.NewClient(config.APIID, config.APIHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: config.SessionFilePath},
	})

	aiClient, err := initAIClient(config)
	if err != nil {
		log.Fatalf("Failed to initialize AI client: %v", err)
	}

	// Start watching the system message file
	go watchSystemMessageFile(aiClient)

	if err := client.Run(ctx, func(ctx context.Context) error {
		if err := authenticateTelegram(ctx, client, config); err != nil {
			return fmt.Errorf("auth failed: %w", err)
		}

		api := client.API()
		return monitorChannels(ctx, api, config, aiClient)
	}); err != nil {
		log.Fatal(err)
	}
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		// trim value
		return strings.TrimSpace(value)
	}
	return fallback
}

func readSystemMessage() (string, error) {
	content, err := ioutil.ReadFile(systemMessageFile)
	if err != nil {
		return "", fmt.Errorf("error reading system message file: %v", err)
	}

	fmt.Println("System message: ", string(content))
	return string(content), nil
}

func watchSystemMessageFile(aiClient AIClient) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalf("Failed to create file watcher: %v", err)
	}
	defer watcher.Close()

	done := make(chan bool)
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Write == fsnotify.Write {
					log.Println("System message file modified. Updating...")
					newMessage, err := readSystemMessage()
					if err != nil {
						log.Printf("Error reading system message: %v", err)
						continue
					}
					updateAIClientSystemMessage(aiClient, newMessage)
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Println("Error watching system message file:", err)
			}
		}
	}()

	err = watcher.Add(systemMessageFile)
	if err != nil {
		log.Fatal(err)
	}
	<-done
}

func updateAIClientSystemMessage(aiClient AIClient, newMessage string) {
	switch c := aiClient.(type) {
	case *ClaudeClient:
		c.SystemMessage = newMessage
	case *ChatGPTClient:
		c.SystemMessage = newMessage
	case *DeepseekClient:
		c.SystemMessage = newMessage
	case *OpenRouterClient:
		c.SystemMessage = newMessage
	case *GeminiClient:
		c.SystemMessage = newMessage
	default:
		log.Println("Unknown AI client type")
	}
	log.Println("AI client system message updated successfully")
}

func initAIClient(config Config) (AIClient, error) {
	systemMessage, err := readSystemMessage()
	if err != nil {
		return nil, fmt.Errorf("failed to read system message: %v", err)
	}

	log.Printf("Initializing AI client with choice: %s", config.AIChoice)

	switch strings.ToLower(config.AIChoice) {
	case "claude":
		log.Println("Initializing Claude client")
		return &ClaudeClient{
			APIKey:         config.AIAPIKey,
			HTTPClient:     &http.Client{},
			SystemMessage:  systemMessage,
			MessageHistory: []Message{},
		}, nil
	case "chatgpt":
		log.Println("Initializing ChatGPT client")
		return &ChatGPTClient{
			APIKey:         config.AIAPIKey,
			HTTPClient:     &http.Client{},
			SystemMessage:  systemMessage,
			MessageHistory: []Message{},
		}, nil
	case "deepseek":
		log.Println("Initializing Deepseek client")
		return &DeepseekClient{
			APIKey:         config.AIAPIKey,
			HTTPClient:     &http.Client{},
			SystemMessage:  systemMessage,
			MessageHistory: []Message{},
		}, nil
	case "openrouter":
		log.Println("Initializing OpenRouter client")
		return &OpenRouterClient{
			APIKey:         config.AIAPIKey,
			HTTPClient:     &http.Client{},
			SystemMessage:  systemMessage,
			MessageHistory: []Message{},
		}, nil
	case "gemini":
		log.Println("Initializing Gemini client")
		return &GeminiClient{
			APIKey:         config.AIAPIKey,
			HTTPClient:     &http.Client{},
			SystemMessage:  systemMessage,
			MessageHistory: []Message{},
		}, nil
	default:
		log.Printf("Unknown AI choice: %s", config.AIChoice)
		return nil, fmt.Errorf("unknown AI choice: %s", config.AIChoice)
	}
}

func authenticateTelegram(ctx context.Context, client *telegram.Client, config Config) error {
	flow := auth.NewFlow(auth.Constant(config.PhoneNumber, "", auth.CodeAuthenticatorFunc(func(ctx context.Context, sentCode *tg.AuthSentCode) (string, error) {
		fmt.Print("Enter code: ")
		var code string
		_, err := fmt.Scan(&code)
		return code, err
	})), auth.SendCodeOptions{})

	return client.Auth().IfNecessary(ctx, flow)
}

func monitorChannels(ctx context.Context, api *tg.Client, config Config, aiClient AIClient) error {
	// Ticker for fetching messages from Telegram
	fetchTicker := time.NewTicker(config.UpdateInterval)
	defer fetchTicker.Stop()

	// Timer for triggering AI send after interval + extensions
	var batchTimer *time.Timer
	var batchTimerChan <-chan time.Time // Channel to select on, nil when timer is not active
	var batchDeadline time.Time

	var mu sync.Mutex
	lastMessageIDs := make(map[string]int)
	messageBuffer := []Message{} // Buffer to hold messages before sending to AI

	// Initialize downloader
	dl := downloader.NewDownloader()

	log.Printf("Monitoring channels. UpdateInterval: %v, AIBatchInterval: %v, AIBatchExtendDuration: %v",
		config.UpdateInterval, config.AIBatchInterval, config.AIBatchExtendDuration)

	// Helper function to stop the timer safely
	stopAndResetTimer := func() {
		if batchTimer != nil {
			if !batchTimer.Stop() {
				// Drain channel if Stop() returns false
				select {
				case <-batchTimerChan:
				default:
				}
			}
		}
		batchTimer = nil
		batchTimerChan = nil
		batchDeadline = time.Time{} // Reset deadline
	}
	defer stopAndResetTimer() // Ensure timer is stopped on exit

	for {
		select {
		case <-ctx.Done():
			log.Println("Context cancelled, stopping monitor loop.")
			return ctx.Err()

		case <-fetchTicker.C: // Fetch messages from Telegram
			// Optional: Check air attack status if not ignored
			if !config.IgnoreAirAttack {
				isAirAttackActive, err := checkAirAttackStatus()
				if err != nil {
					log.Printf("Error checking air attack status: %v", err)
					continue // Skip this fetch cycle on error
				}
				if !isAirAttackActive {
					continue
				}
			}

			var newlyFetchedMessages []Message
			for _, channelInfo := range config.Channels {
				messages, err := getMessages(ctx, api, channelInfo, config.MessageLimit)
				if err != nil {
					log.Printf("Error getting messages for %s: %v", channelInfo.Identifier, err)
					continue
				}

				mu.Lock() // Lock needed for lastMessageIDs access
				newMessages, err := processNewMessages(ctx, api, dl, channelInfo.Identifier, messages, lastMessageIDs)
				mu.Unlock()

				if err != nil {
					log.Printf("Error processing messages for %s: %v", channelInfo.Identifier, err)
				}

				if len(newMessages) > 0 {
					imageCount := 0
					for _, msg := range newMessages {
						imageCount += len(msg.Images)
					}
					if imageCount > 0 {
						log.Printf("Found %d new messages from %s [%d image(s)]", len(newMessages), channelInfo.Identifier, imageCount)
					} else {
						log.Printf("Found %d new messages from %s", len(newMessages), channelInfo.Identifier)
					}
					for _, msg := range newMessages {
						cleanedMsg := cleanString(msg.Content)
						if len(cleanedMsg) > 0 || len(msg.Images) > 0 {
							// Update content with channel info
							msg.Content = fmt.Sprintf("Message from %s:\n%s", channelInfo.Identifier, cleanedMsg)
							newlyFetchedMessages = append(newlyFetchedMessages, msg)
						}
					}
				}
			}

			// Add newly fetched messages and manage the batch timer
			if len(newlyFetchedMessages) > 0 {
				mu.Lock()
				messageBuffer = append(messageBuffer, newlyFetchedMessages...)
				bufferImageCount := 0
				for _, msg := range messageBuffer {
					bufferImageCount += len(msg.Images)
				}
				if bufferImageCount > 0 {
					log.Printf("Added %d messages to buffer. Buffer size: %d [%d image(s)]", len(newlyFetchedMessages), len(messageBuffer), bufferImageCount)
				} else {
					log.Printf("Added %d messages to buffer. Buffer size: %d", len(newlyFetchedMessages), len(messageBuffer))
				}

				var newTimerDuration time.Duration
				if batchTimer == nil { // First message in a potential batch
					newTimerDuration = config.AIBatchInterval
					batchDeadline = time.Now().Add(newTimerDuration)
					log.Printf("Starting batch timer (%v) for the first message. Deadline: %v", newTimerDuration, batchDeadline.Format(time.RFC3339))
				} else { // Subsequent message, extend the deadline
					// Stop the current timer before resetting
					if !batchTimer.Stop() {
						select {
						case <-batchTimerChan:
						default:
						}
					}
					batchDeadline = batchDeadline.Add(config.AIBatchExtendDuration)
					newTimerDuration = time.Until(batchDeadline)
					log.Printf("Extending batch timer by %v. New deadline: %v (in %v)", config.AIBatchExtendDuration, batchDeadline.Format(time.RFC3339), newTimerDuration)
				}

				// Start/Reset the timer with the calculated duration
				batchTimer = time.NewTimer(newTimerDuration)
				batchTimerChan = batchTimer.C

				mu.Unlock()
			}

		case <-batchTimerChan: // Timer fired, batch deadline reached
			mu.Lock()
			if len(messageBuffer) == 0 {
				batchTimer = nil
				batchTimerChan = nil
				batchDeadline = time.Time{}
				mu.Unlock()
				continue
			}

			batchImageCount := 0
			for _, msg := range messageBuffer {
				batchImageCount += len(msg.Images)
			}
			if batchImageCount > 0 {
				log.Printf("Batch deadline reached (%v). Processing %d messages [%d image(s)] from buffer.", batchDeadline.Format(time.RFC3339), len(messageBuffer), batchImageCount)
			} else {
				log.Printf("Batch deadline reached (%v). Processing %d messages from buffer.", batchDeadline.Format(time.RFC3339), len(messageBuffer))
			}
			// Create a copy of the buffer to process and clear the original
			messagesToSend := make([]Message, len(messageBuffer))
			copy(messagesToSend, messageBuffer)
			messageBuffer = []Message{} // Clear the buffer

			// Important: Reset timer state *before* unlocking
			batchTimer = nil
			batchTimerChan = nil
			batchDeadline = time.Time{}

			mu.Unlock()

			// Merge messages and send to AI
			mergedMessage := mergeMessages(messagesToSend)
			if err := handleAIInteraction(ctx, api, config, aiClient, mergedMessage); err != nil {
				log.Printf("Error handling AI interaction: %v", err)
			}
		}
	}
}

func formatMessageForLog(msg Message) string {
	content := msg.Content
	if len(msg.Images) > 0 {
		content = fmt.Sprintf("[%d image(s)] %s", len(msg.Images), content)
	}
	return content
}

func handleAIInteraction(ctx context.Context, api *tg.Client, config Config, aiClient AIClient, message Message) error {
	messages := aiClient.GetMessageHistory()
	fmt.Println("----------------------------------------------------")
	fmt.Println("MESSAGE HISTORY:")
	for i, msg := range messages {
		fmt.Printf("  [%d] %s: %s\n", i, msg.Role, formatMessageForLog(msg))
	}
	fmt.Println("----------------------------------------------------")
	fmt.Printf("LAST MESSAGE: %s\n", formatMessageForLog(message))
	
	// Clean the text content but keep images
	message.Content = cleanString(message.Content)
	
	aiResponse, err := aiClient.SendMessage(ctx, message)
	if err != nil {
		return fmt.Errorf("error sending message to AI: %v", err)
	}

	log.Printf("AI Response: %+v", aiResponse)
	fmt.Println("----------------------------------------------------")
	if config.EnableTelegramSend {
		formattedResponse := formatAIResponse(aiResponse)
		fmt.Println("Sending message to Telegram...")
		if aiResponse.StatusChanged {
			if err := sendToTelegram(ctx, api, sendToChannel, formattedResponse, !aiResponse.Danger); err != nil {
				log.Printf("Error sending message to Telegram: %v", err)
			}
		} else {
			log.Printf("Status not changed, skipping message send")
		}
	} else {
		log.Printf("Telegram send disabled")
	}
	return nil
}

func checkAirAttackStatus() (bool, error) {
	resp, err := http.Get("https://siren.pp.ua/api/v3/alerts/964")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	var alertResp []struct {
		ActiveAlerts []struct {
			Type string `json:"type"`
		} `json:"activeAlerts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&alertResp); err != nil {
		return false, err
	}

	for _, region := range alertResp {
		for _, alert := range region.ActiveAlerts {
			if alert.Type == "AIR" {
				return true, nil
			}
		}
	}
	return false, nil
}

func getMessages(ctx context.Context, api *tg.Client, channelInfo ChannelInfo, limit int) ([]tg.MessageClass, error) {
	var inputPeer tg.InputPeerClass
	var err error

	if channelInfo.IsPrivate {
		channelID, err := strconv.ParseInt(channelInfo.Identifier, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid channel ID: %v", err)
		}
		inputPeer = &tg.InputPeerChannel{
			ChannelID:  channelID,
			AccessHash: 0, // You might need to obtain this value
		}
	} else {
		resolvedPeer, err := api.ContactsResolveUsername(ctx, channelInfo.Identifier)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve username: %v", err)
		}

		for _, chat := range resolvedPeer.Chats {
			if channel, ok := chat.(*tg.Channel); ok {
				inputPeer = channel.AsInputPeer()
				break
			}
		}

		if inputPeer == nil {
			return nil, fmt.Errorf("resolved peer is not a channel")
		}
	}

	messages, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:  inputPeer,
		Limit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get history: %v", err)
	}

	switch msgs := messages.(type) {
	case *tg.MessagesChannelMessages:
		return msgs.Messages, nil
	case *tg.MessagesMessages:
		return msgs.Messages, nil
	case *tg.MessagesMessagesSlice:
		return msgs.Messages, nil
	default:
		return nil, fmt.Errorf("unexpected type for messages: %T", messages)
	}
}

// detectMIMEType detects the MIME type from image bytes
func detectMIMEType(data []byte) string {
	if len(data) < 8 {
		return "image/jpeg" // fallback
	}

	// Check magic bytes
	// JPEG: FF D8 FF
	if data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
		return "image/jpeg"
	}
	// PNG: 89 50 4E 47 0D 0A 1A 0A
	if data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47 {
		return "image/png"
	}
	// GIF: 47 49 46 38
	if data[0] == 0x47 && data[1] == 0x49 && data[2] == 0x46 && data[3] == 0x38 {
		return "image/gif"
	}
	// WebP: 52 49 46 46 ... 57 45 42 50
	if len(data) >= 12 && data[0] == 0x52 && data[1] == 0x49 && data[2] == 0x46 && data[3] == 0x46 &&
		data[8] == 0x57 && data[9] == 0x45 && data[10] == 0x42 && data[11] == 0x50 {
		return "image/webp"
	}

	return "image/jpeg" // fallback to JPEG
}

// downloadImageWithRetry attempts to download an image with retries and fallback thumb sizes
func downloadImageWithRetry(ctx context.Context, api *tg.Client, dl *downloader.Downloader, photo *tg.Photo, msgID int, maxRetries int) (Image, error) {
	thumbSizes := []string{"w", "y", "x", "m", "s"} // Try from largest to smallest

	var lastErr error
	for _, thumbSize := range thumbSizes {
		for attempt := 1; attempt <= maxRetries; attempt++ {
			var buf bytes.Buffer
			_, err := dl.Download(api, &tg.InputPhotoFileLocation{
				ID:            photo.ID,
				AccessHash:    photo.AccessHash,
				FileReference: photo.FileReference,
				ThumbSize:     thumbSize,
			}).Stream(ctx, &buf)

			if err == nil {
				data := buf.Bytes()
				mimeType := detectMIMEType(data)
				log.Printf("Downloaded image from message %d (size: %s, %d bytes, %s)", msgID, thumbSize, len(data), mimeType)
				return Image{
					Data:     data,
					MIMEType: mimeType,
				}, nil
			}

			lastErr = err
			if attempt < maxRetries {
				log.Printf("Retry %d/%d downloading image from message %d (size: %s): %v", attempt, maxRetries, msgID, thumbSize, err)
				time.Sleep(time.Duration(attempt*500) * time.Millisecond) // exponential backoff
			}
		}
		// If all retries failed for this size, try next size
	}

	log.Printf("Failed to download image from message %d after all retries: %v", msgID, lastErr)
	return Image{}, lastErr
}

func processNewMessages(ctx context.Context, api *tg.Client, dl *downloader.Downloader, channelID string, messages []tg.MessageClass, lastMessageIDs map[string]int) ([]Message, error) {
	var newMessages []Message
	latestMessageID := lastMessageIDs[channelID]

	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(*tg.Message)
		if !ok {
			continue
		}

		if msg.ID > latestMessageID {
			date := int64(msg.GetDate())
			unixTimeUTC := time.Unix(date, 0)
			unitTimeInRFC3339 := unixTimeUTC.Format("15:04:05")
			
			content := unitTimeInRFC3339 + "\n" + msg.Message
			var images []Image

			// Check for media
			if media, ok := msg.Media.(*tg.MessageMediaPhoto); ok {
				if photo, ok := media.Photo.(*tg.Photo); ok {
					if img, err := downloadImageWithRetry(ctx, api, dl, photo, msg.ID, 3); err == nil {
						images = append(images, img)
					}
				}
			}

			newMessages = append(newMessages, Message{
				Role:    "user",
				Content: content,
				Images:  images,
			})

			if msg.ID > lastMessageIDs[channelID] {
				lastMessageIDs[channelID] = msg.ID
			}
		}
	}

	return newMessages, nil
}

func mergeMessages(messages []Message) Message {
	var mergedText strings.Builder
	var allImages []Image
	
	for i, msg := range messages {
		if i > 0 {
			mergedText.WriteString("\n\n")
		}
		mergedText.WriteString(msg.Content)
		allImages = append(allImages, msg.Images...)
	}
	
	return Message{
		Role:    "user",
		Content: mergedText.String(),
		Images:  allImages,
	}
}

func sendToTelegram(ctx context.Context, api *tg.Client, channelUsername, message string, silent bool) error {
	resolvedPeer, err := api.ContactsResolveUsername(ctx, channelUsername)
	if err != nil {
		return fmt.Errorf("failed to resolve username: %v", err)
	}

	var channel *tg.Channel
	for _, chat := range resolvedPeer.Chats {
		if ch, ok := chat.(*tg.Channel); ok {
			channel = ch
			break
		}
	}

	if channel == nil {
		return fmt.Errorf("channel not found")
	}

	_, err = api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
		Peer:     channel.AsInputPeer(),
		Message:  message,
		RandomID: rand.Int63(),
		Silent:   silent,
	})

	return err
}

func formatAIResponse(response AIJSONResponse) string {
	emoji := "✅"
	if response.Danger {
		emoji = "🚨"
	}
	return fmt.Sprintf("%s %s", emoji, response.Text)
}

func cleanString(input string) string {
	// Preserve JSON structure while cleaning
	regex := regexp.MustCompile(`[^\p{L}\p{N}\s!@#$%^&*()_+\-=\[\]{};:"'\\|,.<>/?«»—–€₴•]`)

	// Remove invalid characters but preserve JSON formatting
	cleaned := regex.ReplaceAllStringFunc(input, func(m string) string {
		// Allow basic JSON syntax characters
		if strings.ContainsAny(m, "{}[]:,\"") {
			return m
		}
		return ""
	})

	// Remove BOM characters if present
	cleaned = strings.TrimPrefix(cleaned, "\ufeff")

	// Normalize whitespace and trim
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	return strings.TrimSpace(cleaned)
}
