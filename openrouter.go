package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

const (
	maxRetries = 3
	baseDelay  = 2 * time.Second
)

type OpenRouterResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// AddMessageToHistory adds a message to the client's history, maintaining max history size.
func (c *OpenRouterClient) AddMessageToHistory(message Message) {
	c.MessageHistory = append(c.MessageHistory, message)
	if len(c.MessageHistory) > maxMessageHistory {
		c.MessageHistory = c.MessageHistory[1:] // Remove the oldest message
	}
}

// GetMessageHistory returns the current message history.
func (c *OpenRouterClient) GetMessageHistory() []Message {
	return c.MessageHistory
}

// SendMessage implements AIClient.SendMessage for openrouter.ai
func (c *OpenRouterClient) SendMessage(ctx context.Context, message Message) (AIJSONResponse, error) {
	var lastError error
	// Add message to history
	c.AddMessageToHistory(message)

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(math.Pow(2, float64(attempt-1))) * baseDelay
			fmt.Printf("Retrying request (attempt %d/%d) after %v delay...\n", attempt+1, maxRetries, delay)
			time.Sleep(delay)
		}

		var apiMessages []map[string]interface{}

		// System message
		apiMessages = append(apiMessages, map[string]interface{}{
			"role":    "system",
			"content": c.SystemMessage + "\nCurrent time: " + time.Now().Format("15:04:05"),
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
			"model":    "google/gemini-2.5-pro-exp-03-25:free",
			"messages": apiMessages,
		})
		if err != nil {
			return AIJSONResponse{}, fmt.Errorf("marshaling request error: %w", err)
		}

		url := "https://openrouter.ai/api/v1/chat/completions"

		fmt.Printf("Sending request to OpenRouter (attempt %d/%d)\n", attempt+1, maxRetries)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(reqBody))
		if err != nil {
			return AIJSONResponse{}, fmt.Errorf("creating request error: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.APIKey))

		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			lastError = fmt.Errorf("sending request error: %w", err)
			continue
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			lastError = fmt.Errorf("reading response error: %w", err)
			continue
		}

		// Clean the response body by removing any leading/trailing whitespace
		body = []byte(strings.TrimSpace(string(body)))
		fmt.Printf("OpenRouter response: %s\n", string(body))

		// Parse OpenRouter response
		var openRouterResp OpenRouterResponse
		if err := json.Unmarshal(body, &openRouterResp); err != nil {
			lastError = fmt.Errorf("parsing response error: %w, body: %s", err, string(body))
			continue
		}

		if openRouterResp.Error.Message != "" {
			lastError = fmt.Errorf("api error: %s", openRouterResp.Error.Message)
			continue
		}

		if len(openRouterResp.Choices) == 0 {
			lastError = fmt.Errorf("empty choices in response")
			continue
		}

		content := openRouterResp.Choices[0].Message.Content
		
		// Clean content
		content = strings.TrimSpace(content)
		content = strings.TrimPrefix(content, "```json")
		content = strings.TrimPrefix(content, "```")
		content = strings.TrimSuffix(content, "```")
		content = strings.TrimSpace(content)

		var aiResp AIJSONResponse
		if err := json.Unmarshal([]byte(content), &aiResp); err != nil {
			lastError = fmt.Errorf("parsing ai response error: %w, content: %s", err, content)
			continue
		}

		c.AddMessageToHistory(Message{Role: "assistant", Content: fmt.Sprintf("%s Danger: %v StatusChanged: %v", aiResp.Text, aiResp.Danger, aiResp.StatusChanged)})
		return aiResp, nil
	}

	return AIJSONResponse{}, fmt.Errorf("failed after %d retries: %w", maxRetries, lastError)
}
