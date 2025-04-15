package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

const (
	maxRetries = 30
	baseDelay  = 1 * time.Second
)

// NewOpenRouterClient is a constructor for an OpenRouter-based AI client.
func NewOpenRouterClient(apiKey string, systemMessage string) *OpenRouterClient {
	return &OpenRouterClient{
		APIKey:        apiKey,
		SystemMessage: systemMessage,
		HTTPClient:    &http.Client{}, // You can customize your HTTP client as needed.
	}
}

// AddMessageToHistory implements AIClient.AddMessageToHistory
func (c *OpenRouterClient) AddMessageToHistory(message Message) {
	c.MessageHistory = append(c.MessageHistory, message)
	if len(c.MessageHistory) > maxMessageHistory {
		c.MessageHistory = c.MessageHistory[1:]
	}
}

// GetMessageHistory implements AIClient.GetMessageHistory
func (c *OpenRouterClient) GetMessageHistory() []Message {
	return c.MessageHistory
}

// OpenRouterResponse represents the structure of the OpenRouter API response
type OpenRouterResponse struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Choices  []struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message  string `json:"message"`
		Code     int    `json:"code"`
		Metadata struct {
			Raw          string `json:"raw"`
			ProviderName string `json:"provider_name"`
		} `json:"metadata"`
	} `json:"error"`
}

// SendMessage implements AIClient.SendMessage for openrouter.ai
func (c *OpenRouterClient) SendMessage(ctx context.Context, userMessage string) (AIJSONResponse, error) {
	var lastError error
	// Add message to history
	c.AddMessageToHistory(Message{Role: "user", Content: userMessage})

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(math.Pow(2, float64(attempt-1))) * baseDelay
			fmt.Printf("Retrying request (attempt %d/%d) after %v delay...\n", attempt+1, maxRetries, delay)
			time.Sleep(delay)
		}

		reqBody, err := json.Marshal(map[string]interface{}{
			"model": "google/gemini-2.5-pro-exp-03-25:free",
			"messages": append(
				[]Message{{
					Role:    "system",
					Content: c.SystemMessage + "\nCurrent time: " + time.Now().Format("15:04:05"),
				}},
				c.MessageHistory...,
			),
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
			lastError = fmt.Errorf("parsing OpenRouter response error: %w", err)
			continue
		}

		// Check for OpenRouter error response
		if openRouterResp.Error != nil {
			errMsg := fmt.Sprintf("OpenRouter error (code %d): %s", openRouterResp.Error.Code, openRouterResp.Error.Message)
			if openRouterResp.Error.Metadata.Raw != "" {
				errMsg += fmt.Sprintf(" - Provider error: %s", openRouterResp.Error.Metadata.Raw)
			}
			lastError = fmt.Errorf(errMsg)
			continue // Always retry on error responses
		}

		// If we get here, we have a successful response, try to parse it
		if len(openRouterResp.Choices) == 0 {
			lastError = fmt.Errorf("no choices in response")
			continue
		}

		content := openRouterResp.Choices[0].Message.Content

		// Clean and parse the content
		content = strings.TrimSpace(content)
		content = strings.TrimPrefix(content, "```json")
		content = strings.TrimPrefix(content, "```yaml")
		content = strings.TrimPrefix(content, "```")
		content = strings.TrimSuffix(content, "```")
		content = strings.TrimSpace(content)

		// Parse the actual AIJSONResponse from the content
		var result AIJSONResponse
		if err := json.Unmarshal([]byte(content), &result); err != nil {
			lastError = fmt.Errorf("parsing AI response error: %w\nContent: %s", err, content)
			continue
		}

		// Success! Add to message history and return immediately
		assistantMsg := fmt.Sprintf("%s (Principle: %s, Danger: %v, StatusChanged: %v)",
			result.Text, result.Principle, result.Danger, result.StatusChanged)
		c.AddMessageToHistory(Message{Role: "assistant", Content: assistantMsg})
		return result, nil
	}

	return AIJSONResponse{}, fmt.Errorf("max retries exceeded, last error: %v", lastError)
}
