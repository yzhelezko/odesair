package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"golang.org/x/xerrors"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

const maxMessageHistory = 30
const systemMessageFile = "system_message.txt"

var lastMessage string
var lastMessageIDs = make(map[string]int)

type ChannelInfo struct {
	Identifier string // Can be username or channel ID
	IsPrivate  bool
}

type Config struct {
	APIID           int
	APIHash         string
	PhoneNumber     string
	Channels        []ChannelInfo
	MessageLimit    int
	SessionFilePath string
	UpdateInterval  time.Duration
	ClaudeAPIKey    string
	AIChoice        string // "claude" or "chatgpt"
	ChatGPTAPIKey   string
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

type AIClient interface {
	SendMessage(ctx context.Context, message string) (AIJSONResponse, error)
	AddMessageToHistory(message Message)
}

type ClaudeClient struct {
	APIKey         string
	HTTPClient     *http.Client
	SystemMessage  string
	MessageHistory []Message
}

type ClaudeRequest struct {
	Model     string    `json:"model"`
	System    string    `json:"system"`
	Messages  []Message `json:"messages"`
	MaxTokens int       `json:"max_tokens"`
}

type ClaudeResponse struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
}

type ChatGPTClient struct {
	APIKey         string
	HTTPClient     *http.Client
	SystemMessage  string
	MessageHistory []Message
}

type ChatGPTRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
}

type ChatGPTResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

type AlertResponse []struct {
	RegionID      string        `json:"regionId"`
	RegionType    string        `json:"regionType"`
	RegionName    string        `json:"regionName"`
	RegionEngName string        `json:"regionEngName"`
	LastUpdate    time.Time     `json:"lastUpdate"`
	ActiveAlerts  []ActiveAlert `json:"activeAlerts"`
}

type ActiveAlert struct {
	RegionID   string    `json:"regionId"`
	RegionType string    `json:"regionType"`
	Type       string    `json:"type"`
	LastUpdate time.Time `json:"lastUpdate"`
}

func NewClaudeClient(apiKey string, systemMessage string) *ClaudeClient {
	return &ClaudeClient{
		APIKey:         apiKey,
		HTTPClient:     &http.Client{},
		SystemMessage:  systemMessage,
		MessageHistory: []Message{},
	}
}

func (c *ClaudeClient) AddMessageToHistory(message Message) {
	fmt.Println("Message History: ", c.MessageHistory)

	fmt.Println("Message to add: ", message)
	c.MessageHistory = append(c.MessageHistory, message)

	// Ensure we don't exceed maxMessageHistory
	for len(c.MessageHistory) > maxMessageHistory {
		c.MessageHistory = c.MessageHistory[1:]
	}

	fmt.Println("Message History result: ", c.MessageHistory)
}

func (c *ClaudeClient) SendMessage(ctx context.Context, message string) (AIJSONResponse, error) {
	var aiResp AIJSONResponse
	maxRetries := 3
	var lastErr error

	c.AddMessageToHistory(Message{Role: "user", Content: message})

	for i := 0; i < maxRetries; i++ {
		url := "https://api.anthropic.com/v1/messages"

		requestBody, err := json.Marshal(ClaudeRequest{
			Model:     "claude-3-5-sonnet-20240620",
			System:    c.SystemMessage,
			Messages:  c.MessageHistory,
			MaxTokens: 1000,
		})

		fmt.Println("System message: ", c.SystemMessage)
		fmt.Println("Message history: ")
		for i, message := range c.MessageHistory {
			fmt.Println("Message: ", message, ", index: ", i)
		}

		if err != nil {
			return aiResp, fmt.Errorf("error marshaling request: %v", err)
		}

		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(requestBody))
		if err != nil {
			return aiResp, fmt.Errorf("error creating request: %v", err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", c.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")

		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("error sending request: %v", err)
			time.Sleep(time.Second * time.Duration(i+1))
			continue
		}
		defer resp.Body.Close()

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			lastErr = fmt.Errorf("error reading response body: %v", err)
			time.Sleep(time.Second * time.Duration(i+1))
			continue
		}

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("API request failed with status code %d: %s", resp.StatusCode, string(body))
			time.Sleep(time.Second * time.Duration(i+1))
			continue
		}

		var claudeResp ClaudeResponse
		err = json.Unmarshal(body, &claudeResp)
		if err != nil {
			lastErr = fmt.Errorf("error unmarshaling response: %v", err)
			time.Sleep(time.Second * time.Duration(i+1))
			continue
		}

		if len(claudeResp.Content) > 0 {
			err = json.Unmarshal([]byte(claudeResp.Content[0].Text), &aiResp)
			if err != nil {
				lastErr = fmt.Errorf("error unmarshaling JSON content: %v", err)
				time.Sleep(time.Second * time.Duration(i+1))
				continue
			}

			if validateAIResponse(aiResp) {
				return aiResp, nil
			}
			lastErr = errors.New("invalid Claude response format")
		} else {
			lastErr = errors.New("no content in Claude response")
		}

		time.Sleep(time.Second * time.Duration(i+1))
	}

	c.AddMessageToHistory(Message{Role: "assistant", Content: aiResp.Text})

	return aiResp, fmt.Errorf("failed after %d retries. Last error: %v", maxRetries, lastErr)
}

func NewChatGPTClient(apiKey string, systemMessage string) *ChatGPTClient {
	return &ChatGPTClient{
		APIKey:         apiKey,
		HTTPClient:     &http.Client{},
		SystemMessage:  systemMessage,
		MessageHistory: []Message{},
	}
}

func (c *ChatGPTClient) AddMessageToHistory(message Message) {
	c.MessageHistory = append(c.MessageHistory, message)
	if len(c.MessageHistory) > maxMessageHistory {
		c.MessageHistory = c.MessageHistory[1:]
	}
}

func (c *ChatGPTClient) SendMessage(ctx context.Context, message string) (AIJSONResponse, error) {
	var aiResp AIJSONResponse
	maxRetries := 3
	var lastErr error

	c.AddMessageToHistory(Message{Role: "user", Content: message})

	for i := 0; i < maxRetries; i++ {
		url := "https://api.openai.com/v1/chat/completions"
		sysMessage := c.SystemMessage + "\n\nТвой последний ответ:\n" + lastMessage
		requestBody, err := json.Marshal(ChatGPTRequest{
			Model: "gpt-3.5-turbo",
			Messages: append([]Message{
				{Role: "system", Content: sysMessage},
			}, c.MessageHistory...),
		})
		if err != nil {
			return aiResp, fmt.Errorf("error marshaling request: %v", err)
		}

		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(requestBody))
		if err != nil {
			return aiResp, fmt.Errorf("error creating request: %v", err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.APIKey)

		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("error sending request: %v", err)
			time.Sleep(time.Second * time.Duration(i+1))
			continue
		}
		defer resp.Body.Close()

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			lastErr = fmt.Errorf("error reading response body: %v", err)
			time.Sleep(time.Second * time.Duration(i+1))
			continue
		}

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("API request failed with status code %d: %s", resp.StatusCode, string(body))
			time.Sleep(time.Second * time.Duration(i+1))
			continue
		}

		var chatGPTResp ChatGPTResponse
		err = json.Unmarshal(body, &chatGPTResp)
		if err != nil {
			lastErr = fmt.Errorf("error unmarshaling response: %v", err)
			time.Sleep(time.Second * time.Duration(i+1))
			continue
		}

		if len(chatGPTResp.Choices) > 0 {
			err = json.Unmarshal([]byte(chatGPTResp.Choices[0].Message.Content), &aiResp)
			if err != nil {
				lastErr = fmt.Errorf("error unmarshaling JSON content: %v", err)
				time.Sleep(time.Second * time.Duration(i+1))
				continue
			}

			if validateAIResponse(aiResp) {
				return aiResp, nil
			}
			lastErr = errors.New("invalid ChatGPT response format")
		} else {
			lastErr = errors.New("no content in ChatGPT response")
		}

		time.Sleep(time.Second * time.Duration(i+1))
	}

	return aiResp, fmt.Errorf("failed after %d retries. Last error: %v", maxRetries, lastErr)
}

func validateAIResponse(resp AIJSONResponse) bool {
	return resp.Text != "" && (resp.Danger == true || resp.Danger == false) && (resp.StatusChanged == true || resp.StatusChanged == false)
}

func checkAirAttackStatus() (bool, error) {
	resp, err := http.Get("https://siren.pp.ua/api/v3/alerts/964")
	if err != nil {
		return false, fmt.Errorf("error making request to air attack API: %v", err)
	}
	defer resp.Body.Close()

	var alertResp AlertResponse
	if err := json.NewDecoder(resp.Body).Decode(&alertResp); err != nil {
		return false, fmt.Errorf("error decoding air attack API response: %v", err)
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

func sendToTelegram(ctx context.Context, api *tg.Client, channelUsername string, message string, silent bool) error {
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

	randomID := rand.Int63()

	_, err = api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
		Peer:     channel.AsInputPeer(),
		Message:  message,
		RandomID: randomID,
		Silent:   silent,
	})

	return err
}

func readSystemMessage() (string, error) {
	content, err := ioutil.ReadFile(systemMessageFile)
	if err != nil {
		return "", fmt.Errorf("error reading system message file: %v", err)
	}
	return string(content), nil
}

func watchSystemMessageFile(aiClient AIClient) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("error creating file watcher: %v", err)
	}

	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Write == fsnotify.Write {
					newMessage, err := readSystemMessage()
					if err != nil {
						fmt.Printf("Error reading updated system message: %v\n", err)
						continue
					}
					switch c := aiClient.(type) {
					case *ClaudeClient:
						c.SystemMessage = newMessage
					case *ChatGPTClient:
						c.SystemMessage = newMessage
					}
					fmt.Println("System message updated")
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				fmt.Printf("Error watching system message file: %v\n", err)
			}
		}
	}()

	err = watcher.Add(systemMessageFile)
	if err != nil {
		return fmt.Errorf("error adding file to watcher: %v", err)
	}

	return nil
}

func main() {
	config := Config{
		APIID:       0,  // your app id here
		APIHash:     "", // your app hash here
		PhoneNumber: "", // your phone number here
		Channels: []ChannelInfo{
			{Identifier: "odessa_infonews", IsPrivate: false},
			{Identifier: "xydessa_live", IsPrivate: false},
			{Identifier: "freechat_odesa", IsPrivate: false},
		},
		MessageLimit:    4,
		SessionFilePath: "tdlib-session",
		UpdateInterval:  5 * time.Second,
		ClaudeAPIKey:    "claude-api-key-here",
		AIChoice:        "claude", // claude or "chatgpt"
		ChatGPTAPIKey:   "chatgpt-api-key-here",
	}

	if os.Getenv("CLAUDE_API_KEY") != "" {
		config.ClaudeAPIKey = os.Getenv("CLAUDE_API_KEY")
		config.AIChoice = "claude"
	}
	if os.Getenv("CHATGPT_API_KEY") != "" {
		config.ChatGPTAPIKey = os.Getenv("CHATGPT_API_KEY")
		config.AIChoice = "chatgpt"
	}
	if os.Getenv("PHONE_NUMBER") != "" {
		config.PhoneNumber = os.Getenv("PHONE_NUMBER")
	}
	if os.Getenv("APPHASH") != "" {
		config.APIHash = os.Getenv("APPHASH")
	}
	if os.Getenv("APPID") != "" {
		appid, _ := fmt.Printf("%s", os.Getenv("APPID"))
		config.APIID = appid
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	client := telegram.NewClient(config.APIID, config.APIHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: config.SessionFilePath},
	})

	systemMessage, err := readSystemMessage()
	if err != nil {
		log.Fatalf("Failed to read system message: %v", err)
	}

	var aiClient AIClient
	if config.AIChoice == "chatgpt" {
		aiClient = NewChatGPTClient(config.ChatGPTAPIKey, systemMessage)
	} else {
		aiClient = NewClaudeClient(config.ClaudeAPIKey, systemMessage)
	}

	err = watchSystemMessageFile(aiClient)
	if err != nil {
		log.Fatalf("Failed to set up system message file watcher: %v", err)
	}

	codeAuth := auth.CodeAuthenticatorFunc(func(ctx context.Context, sentCode *tg.AuthSentCode) (string, error) {
		fmt.Print("Enter code: ")
		var code string
		_, err := fmt.Scan(&code)
		if err != nil {
			return "", xerrors.Errorf("failed to scan code: %w", err)
		}
		return code, nil
	})

	flow := auth.NewFlow(
		auth.Constant(config.PhoneNumber, "", codeAuth),
		auth.SendCodeOptions{},
	)

	if err := client.Run(ctx, func(ctx context.Context) error {
		if err := client.Auth().IfNecessary(ctx, flow); err != nil {
			return xerrors.Errorf("auth failed: %w", err)
		}

		api := client.API()

		var wg sync.WaitGroup
		wg.Add(1)

		go func() {
			defer wg.Done()
			isFirstRun := true
			for {
				select {
				case <-ctx.Done():
					fmt.Println("Shutting down...")
					return
				default:
					isAirAttackActive, err := checkAirAttackStatus()
					if err != nil {
						fmt.Printf("Error checking air attack status: %v\n", err)
						time.Sleep(config.UpdateInterval)
						continue
					}

					if !isFirstRun && isAirAttackActive {
						fmt.Println("No active air attack. Skipping message check.")
						time.Sleep(config.UpdateInterval)
						continue
					}

					allMessages := ""
					for _, channelInfo := range config.Channels {
						messages, err := getMessages(ctx, api, channelInfo, config.MessageLimit)
						if err != nil {
							fmt.Printf("Error getting messages for %s: %v\n", channelInfo.Identifier, err)
							continue
						}

						newMessages := processNewMessages(channelInfo.Identifier, messages)
						if len(newMessages) > 0 {
							mergedText := mergeMessages(newMessages)
							fmt.Printf("\nNew message from %s:\n%s\n", channelInfo.Identifier, mergedText)
							allMessages += fmt.Sprintf("Messages from %s:\n%s\n\n", channelInfo.Identifier, mergedText)
						}
					}

					if allMessages != "" {
						// Send gathered messages to AI
						fmt.Println("Sending message to AI...")
						aiResponse, err := aiClient.SendMessage(ctx, allMessages)
						if err != nil {
							fmt.Printf("Error sending message to AI: %v\n", err)
						} else {
							fmt.Printf("AI's response: %+v\n", aiResponse)

							// Send AI's response to the Telegram channel
							emoj := "✅"
							silent := true
							if aiResponse.Danger {
								emoj = "🚨"
								silent = false
							}
							fmt.Println(silent)
							if aiResponse.StatusChanged {
								responseMessage := fmt.Sprintf("%[3]s %[1]s\n\nОпасность: %[2]v %[3]s", aiResponse.Text, aiResponse.Danger, emoj)
								lastMessage = responseMessage
								// err = sendToTelegram(ctx, api, "odesair", responseMessage, silent)
								// if err != nil {
								// 	fmt.Printf("Error sending message to Telegram: %v\n", err)
								// 	fmt.Printf("Message content: %s\n", responseMessage)
								// } else {
								// 	fmt.Printf("Successfully sent message to Telegram channel 'odesair'\n")
								// }
							}
						}
					}

					isFirstRun = false
					time.Sleep(config.UpdateInterval)
				}
			}
		}()

		wg.Wait()
		return nil
	}); err != nil {
		log.Fatal(err)
	}
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
			AccessHash: 0,
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

func processNewMessages(channelID string, messages []tg.MessageClass) []string {
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
	for i, message := range messages {
		messages[i] = fmt.Sprintf("message: %s", message)
	}

	return strings.Join(messages, "\n")
}
