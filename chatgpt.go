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

func (c *ChatGPTClient) AddMessageToHistory(message Message) {
	c.MessageHistory = append(c.MessageHistory, message)
	if len(c.MessageHistory) > maxMessageHistory {
		c.MessageHistory = c.MessageHistory[1:]
	}
}

func (c *ChatGPTClient) GetMessageHistory() []Message {
	return c.MessageHistory
}

type responseFormat struct {
	Type string `json:"type"`
}

func (c *ChatGPTClient) SendMessage(ctx context.Context, message string) (AIJSONResponse, error) {
	c.AddMessageToHistory(Message{Role: "user", Content: message})

	url := "https://api.openai.com/v1/chat/completions"
	reqBody, err := json.Marshal(map[string]interface{}{
		"model":           "gpt-4o",
		"response_format": responseFormat{Type: "json_object"},
		"messages": append([]Message{
			{Role: "system", Content: c.SystemMessage + "\n Текущее время: " + time.Now().Format("15:04:05")},
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
	c.AddMessageToHistory(Message{Role: "assistant", Content: fmt.Sprintf("%s Danger: %v StatusChanged: %v", aiResp.Text, aiResp.Danger, aiResp.StatusChanged)})
	return aiResp, nil
}
