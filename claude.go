package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io/ioutil"
	"net/http"
)

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
