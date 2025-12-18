package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
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

func (c *ChatGPTClient) SendMessage(ctx context.Context, message Message) (AIJSONResponse, error) {
	c.AddMessageToHistory(message)

	url := "https://api.openai.com/v1/chat/completions"

	var apiMessages []map[string]interface{}

	// System message
	apiMessages = append(apiMessages, map[string]interface{}{
		"role":    "system",
		"content": c.SystemMessage + "\n Текущее время: " + time.Now().Format("15:04:05"),
	})

	// History messages
	for _, msg := range c.MessageHistory {
		if len(msg.Images) > 0 {
			var contentParts []map[string]interface{}

			// Add text if present
			if msg.Content != "" {
				contentParts = append(contentParts, map[string]interface{}{
					"type": "text",
					"text": msg.Content,
				})
			}

			// Add images
			for _, img := range msg.Images {
				contentParts = append(contentParts, map[string]interface{}{
					"type": "image_url",
					"image_url": map[string]string{
						"url": fmt.Sprintf("data:%s;base64,%s", img.MIMEType, base64.StdEncoding.EncodeToString(img.Data)),
					},
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
		"model":           "o3-mini",
		"response_format": responseFormat{Type: "json_object"},
		"messages":        apiMessages,
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

	body, err := io.ReadAll(resp.Body)
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
	if len(chatGPTResp.Choices) > 0 {
		if err := json.Unmarshal([]byte(chatGPTResp.Choices[0].Message.Content), &aiResp); err != nil {
			return AIJSONResponse{}, err
		}
		c.AddMessageToHistory(Message{Role: "assistant", Content: fmt.Sprintf("%s Danger: %v StatusChanged: %v", aiResp.Text, aiResp.Danger, aiResp.StatusChanged)})
		return aiResp, nil
	}
	return AIJSONResponse{}, fmt.Errorf("no response from chatgpt")
}
