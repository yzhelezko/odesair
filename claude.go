package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
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

func (c *ClaudeClient) SendMessage(ctx context.Context, message Message) (AIJSONResponse, error) {
	c.AddMessageToHistory(message)

	url := "https://api.anthropic.com/v1/messages"

	var apiMessages []map[string]interface{}

	for _, msg := range c.MessageHistory {
		if len(msg.Images) > 0 {
			var contentParts []map[string]interface{}
			
			// Add images
			for _, img := range msg.Images {
				contentParts = append(contentParts, map[string]interface{}{
					"type": "image",
					"source": map[string]string{
						"type":       "base64",
						"media_type": img.MIMEType,
						"data":       base64.StdEncoding.EncodeToString(img.Data),
					},
				})
			}

			// Add text
			if msg.Content != "" {
				contentParts = append(contentParts, map[string]interface{}{
					"type": "text",
					"text": msg.Content,
				})
			}

			apiMessages = append(apiMessages, map[string]interface{}{
				"role":    msg.Role,
				"content": contentParts,
			})
		} else {
			apiMessages = append(apiMessages, map[string]interface{}{
				"role":    msg.Role,
				"content": msg.Content,
			})
		}
	}

	reqBody, err := json.Marshal(map[string]interface{}{
		"model":    "claude-3-opus-20240229",
		"system":   c.SystemMessage,
		"messages": apiMessages,
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

	body, err := io.ReadAll(resp.Body)
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

	if len(claudeResp.Content) == 0 {
		return AIJSONResponse{}, fmt.Errorf("empty response from claude")
	}

	var aiResp AIJSONResponse
	if err := json.Unmarshal([]byte(claudeResp.Content[0].Text), &aiResp); err != nil {
		return AIJSONResponse{}, err
	}

	c.AddMessageToHistory(Message{Role: "assistant", Content: fmt.Sprintf("%s Danger: %v StatusChanged: %v", aiResp.Text, aiResp.Danger, aiResp.StatusChanged)})

	return aiResp, nil
}
