package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type DeepseekClient struct {
	APIKey         string
	HTTPClient     *http.Client
	SystemMessage  string
	MessageHistory []Message
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

func (c *DeepseekClient) SendMessage(ctx context.Context, message Message) (AIJSONResponse, error) {
	c.AddMessageToHistory(message)

	url := "https://api.deepseek.com/v1/chat/completions"

	var apiMessages []map[string]interface{}

	// System message
	apiMessages = append(apiMessages, map[string]interface{}{
		"role":    "system",
		"content": c.SystemMessage + "\n Current time: " + time.Now().Format("15:04:05"),
	})

	// History messages
	for _, msg := range c.MessageHistory {
		if len(msg.Images) > 0 {
			var contentParts []map[string]interface{}
			
			// Add text
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
		"model":    "deepseek-chat",
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

	// Handle UTF-8 BOM and clean response body
	body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))

	var deepseekResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &deepseekResp); err != nil {
		return AIJSONResponse{}, fmt.Errorf("failed to parse API response: %w (body: %q)", err, string(body))
	}

	if len(deepseekResp.Choices) == 0 {
		return AIJSONResponse{}, fmt.Errorf("no choices in response: %s", deepseekResp.Error.Message)
	}

	// Clean and parse the JSON content
	content := strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(deepseekResp.Choices[0].Message.Content, "```"), "```json"))

	if !strings.HasPrefix(content, "{") {
		return AIJSONResponse{}, fmt.Errorf("unexpected response format, expected JSON object but got: %q", content)
	}

	var aiResp AIJSONResponse
	if err := json.Unmarshal([]byte(content), &aiResp); err != nil {
		return AIJSONResponse{}, fmt.Errorf("failed to unmarshal JSON content: %w (content: %q)", err, content)
	}

	c.AddMessageToHistory(Message{Role: "assistant", Content: fmt.Sprintf("%s Danger: %v StatusChanged: %v", aiResp.Text, aiResp.Danger, aiResp.StatusChanged)})

	return aiResp, nil
}
