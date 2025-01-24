package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"
)

type DeepseekClient struct {
	APIKey         string
	HTTPClient     *http.Client
	SystemMessage  string
	MessageHistory []Message
}

func NewDeepseekClient(apiKey string, systemMessage string) *DeepseekClient {
	return &DeepseekClient{
		APIKey:         apiKey,
		HTTPClient:     &http.Client{Timeout: 30 * time.Second},
		SystemMessage:  systemMessage,
		MessageHistory: []Message{},
	}
}

func (c *DeepseekClient) AddMessageToHistory(message Message) {
	c.MessageHistory = append(c.MessageHistory, message)
	if len(c.MessageHistory) > maxMessageHistory {
		c.MessageHistory = c.MessageHistory[1:]
	}
}

func (c *DeepseekClient) GetMessageHistory() []Message {
	return c.MessageHistory
}

func (c *DeepseekClient) SendMessage(ctx context.Context, message string) (AIJSONResponse, error) {
	c.AddMessageToHistory(Message{Role: "user", Content: message})

	url := "https://api.deepseek.com/v1/chat/completions"
	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "deepseek-chat",
		"messages": append([]Message{
			{Role: "system", Content: c.SystemMessage + "\n Current time: " + time.Now().Format("15:04:05")},
		}, c.MessageHistory...),
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

	var deepseekResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &deepseekResp); err != nil {
		return AIJSONResponse{}, err
	}

	var aiResp AIJSONResponse
	if err := json.Unmarshal([]byte(deepseekResp.Choices[0].Message.Content), &aiResp); err != nil {
		return AIJSONResponse{}, err
	}
	c.AddMessageToHistory(Message{Role: "assistant", Content: fmt.Sprintf("%s Danger: %v StatusChanged: %v", aiResp.Text, aiResp.Danger, aiResp.StatusChanged)})
	return aiResp, nil
}
