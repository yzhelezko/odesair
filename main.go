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
	"github.com/gotd/td/tg"
)

const (
	maxMessageHistory = 10
	systemMessageFile = "system_message.txt"
)

type Config struct {
	APIID              int
	APIHash            string
	PhoneNumber        string
	Channels           []ChannelInfo
	MessageLimit       int
	SessionFilePath    string
	UpdateInterval     time.Duration
	AIChoice           string
	AIAPIKey           string
	EnableTelegramSend bool
	IgnoreAirAttack    bool
}

type ChannelInfo struct {
	Identifier string
	IsPrivate  bool
}

type AIClient interface {
	SendMessage(ctx context.Context, message string) (AIJSONResponse, error)
	AddMessageToHistory(message Message)
	GetMessageHistory() []Message
}

type AIJSONResponse struct {
	Text          string `json:"text"`
	Danger        bool   `json:"danger"`
	StatusChanged bool   `json:"statusChanged"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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

func loadConfig() Config {
	appID, _ := strconv.Atoi(getEnv("APPID", ""))
	return Config{
		APIID:       appID,
		APIHash:     getEnv("APPHASH", ""),
		PhoneNumber: getEnv("PHONE_NUMBER", ""),
		Channels: []ChannelInfo{
			{Identifier: "odessa_infonews", IsPrivate: false},
			{Identifier: "xydessa_live", IsPrivate: false},
			{Identifier: "freechat_odesa", IsPrivate: false},
		},
		MessageLimit:       5,
		SessionFilePath:    "tdlib-session",
		UpdateInterval:     5 * time.Second,
		AIChoice:           getEnv("AI_CHOICE", "chatgpt"),
		AIAPIKey:           getEnv("API_KEY", ""),
		EnableTelegramSend: true,
		IgnoreAirAttack:    false,
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
	ticker := time.NewTicker(config.UpdateInterval)
	defer ticker.Stop()

	var mu sync.Mutex
	lastMessageIDs := make(map[string]int)
	isFirstRun := true

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if !config.IgnoreAirAttack {
				isAirAttackActive, err := checkAirAttackStatus()
				if err != nil {
					log.Printf("Error checking air attack status: %v", err)
					continue
				}
				if !isAirAttackActive {
					log.Println("No active air attack. Skipping message check.")
					continue
				}
			}

			allMessages := []string{}
			for _, channelInfo := range config.Channels {
				messages, err := getMessages(ctx, api, channelInfo, config.MessageLimit)
				if err != nil {
					log.Printf("Error getting messages for %s: %v", channelInfo.Identifier, err)
					continue
				}

				mu.Lock()
				newMessages := processNewMessages(channelInfo.Identifier, messages, lastMessageIDs)
				mu.Unlock()

				if len(newMessages) > 0 {
					for _, msg := range newMessages {
						allMessages = append(allMessages, fmt.Sprintf("Message from %s:\n%s", channelInfo.Identifier, msg))
					}
				}
			}

			if len(allMessages) > 0 {
				if isFirstRun {
					mergedMessage := mergeMessages(allMessages)
					if err := handleAIInteraction(ctx, api, config, aiClient, mergedMessage); err != nil {
						log.Printf("Error handling AI interaction: %v", err)
					}
					isFirstRun = false
				} else {
					for _, msg := range allMessages {
						if err := handleAIInteraction(ctx, api, config, aiClient, msg); err != nil {
							log.Printf("Error handling AI interaction: %v", err)
						}
					}
				}
			}
		}
	}
}

func handleAIInteraction(ctx context.Context, api *tg.Client, config Config, aiClient AIClient, message string) error {
	aiResponse, err := aiClient.SendMessage(ctx, cleanString(message))
	if err != nil {
		return fmt.Errorf("error sending message to AI: %v", err)
	}

	aiClient.AddMessageToHistory(Message{Role: "user", Content: cleanString(message)})
	aiClient.AddMessageToHistory(Message{Role: "assistant", Content: aiResponse.Text})

	log.Printf("AI Response: %+v", aiResponse)

	if config.EnableTelegramSend {
		formattedResponse := formatAIResponse(aiResponse)
		fmt.Println("Sending message to Telegram...")
		if err := sendToTelegram(ctx, api, "odesair", formattedResponse, !aiResponse.Danger); err != nil {
			log.Printf("Error sending message to Telegram: %v", err)
		}
	} else {
		log.Printf("Telegram send disabled")
	}

	fmt.Println("Message history:")
	messages := aiClient.GetMessageHistory()
	fmt.Println(messages)
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

func processNewMessages(channelID string, messages []tg.MessageClass, lastMessageIDs map[string]int) []string {
	var newMessages []string
	latestMessageID := lastMessageIDs[channelID]

	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(*tg.Message)
		if !ok {
			continue
		}

		if msg.ID > latestMessageID {
			newMessages = append(newMessages, msg.Message)
			if msg.ID > lastMessageIDs[channelID] {
				lastMessageIDs[channelID] = msg.ID
			}
		}
	}

	return newMessages
}

func mergeMessages(messages []string) string {
	return strings.Join(messages, "\n\n")
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
	return fmt.Sprintf("%s %s\n\nОпасность: %v", emoji, response.Text, response.Danger)
}

func (c *ClaudeClient) AddMessageToHistory(message Message) {
	c.MessageHistory = append(c.MessageHistory, message)
	if len(c.MessageHistory) > maxMessageHistory {
		c.MessageHistory = c.MessageHistory[1:]
	}
}

func (c *ClaudeClient) GetMessageHistory() []Message {
	return c.MessageHistory
}

func (c *ClaudeClient) SendMessage(ctx context.Context, message string) (AIJSONResponse, error) {
	url := "https://api.anthropic.com/v1/messages"
	reqBody, err := json.Marshal(map[string]interface{}{
		"model":    "claude-3-opus-20240229",
		"system":   c.SystemMessage,
		"messages": c.MessageHistory,
	})
	if err != nil {
		return AIJSONResponse{}, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return AIJSONResponse{}, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return AIJSONResponse{}, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return AIJSONResponse{}, err
	}

	var claudeResp struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &claudeResp); err != nil {
		return AIJSONResponse{}, err
	}

	var aiResp AIJSONResponse
	if err := json.Unmarshal([]byte(claudeResp.Content[0].Text), &aiResp); err != nil {
		return AIJSONResponse{}, err
	}

	return aiResp, nil
}

func (c *ChatGPTClient) AddMessageToHistory(message Message) {
	c.MessageHistory = append(c.MessageHistory, message)
	if len(c.MessageHistory) > maxMessageHistory {
		c.MessageHistory = c.MessageHistory[1:]
	}
}

func (c *ChatGPTClient) GetMessageHistory() []Message {
	return c.MessageHistory
}

func (c *ChatGPTClient) SendMessage(ctx context.Context, message string) (AIJSONResponse, error) {
	messages := c.MessageHistory
	messages = append(messages, Message{Role: "user", Content: message})

	url := "https://api.openai.com/v1/chat/completions"
	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-4o-mini-2024-07-18",
		"messages": append([]Message{
			{Role: "system", Content: c.SystemMessage},
		}, messages...),
	})
	if err != nil {
		return AIJSONResponse{}, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return AIJSONResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.APIKey))
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return AIJSONResponse{}, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return AIJSONResponse{}, err
	}

	var chatGPTResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &chatGPTResp); err != nil {
		return AIJSONResponse{}, err
	}

	var aiResp AIJSONResponse
	if err := json.Unmarshal([]byte(chatGPTResp.Choices[0].Message.Content), &aiResp); err != nil {
		return AIJSONResponse{}, err
	}

	return aiResp, nil
}

func cleanString(input string) string {
	// Regular expression to match:
	// - numbers
	// - Russian, Ukrainian, and English characters
	// - spaces and newlines
	// - common symbols
	regex := regexp.MustCompile(`[^0-9a-zA-Zа-яА-ЯіІїЇєЄґҐ'\s!@#$%^&*()_+\-=\[\]{};:"\\|,.<>/?]+`)

	// Replace all non-matching characters with an empty string
	cleaned := regex.ReplaceAllString(input, "")

	return cleaned
}
