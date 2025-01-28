package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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

// SendMessage implements AIClient.SendMessage for openrouter.ai
func (c *OpenRouterClient) SendMessage(ctx context.Context, userMessage string) (AIJSONResponse, error) {

	c.AddMessageToHistory(Message{Role: "user", Content: userMessage})

    reqBody, err := json.Marshal(map[string]interface{}{
        "model": "google/gemini-2.0-flash-thinking-exp:free",
        "messages": append(
            []Message{{
                Role:    "system",
                Content: c.SystemMessage + "\nCurrent time: " + time.Now().Format("15:04:05"),
            }},
            c.MessageHistory...,
        ),
        // If you need a specific response format or additional params, include them here:
        // "temperature": 0.7,
        // "response_format": map[string]string{"type": "json_object"},
    })
    if err != nil {
        return AIJSONResponse{}, fmt.Errorf("json.Marshal error: %w", err)
    }

    // 3. Create the HTTP request
    url := "https://openrouter.ai/api/v1/chat/completions"
    req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(reqBody))
    if err != nil {
        return AIJSONResponse{}, fmt.Errorf("creating request error: %w", err)
    }

    // 4. Set the headers for OpenRouter
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.APIKey))
    // Optional: OpenRouter sometimes supports additional headers (e.g., "X-Title").

    // 5. Execute the request
    resp, err := c.HTTPClient.Do(req)
    if err != nil {
        return AIJSONResponse{}, fmt.Errorf("HTTP request error: %w", err)
    }
    defer resp.Body.Close()

    // 6. Read response
    body, err := io.ReadAll(resp.Body)
    if err != nil {
        return AIJSONResponse{}, fmt.Errorf("reading response body error: %w", err)
    }

    // 7. Parse the JSON response
    // This structure matches an OpenAI-style Chat Completion response,
    // which OpenRouter generally follows. Adjust as necessary.
    var openRouterResp struct {
        Choices []struct {
            Message struct {
                Content string `json:"content"`
            } `json:"message"`
        } `json:"choices"`
    }
    if err := json.Unmarshal(body, &openRouterResp); err != nil {
        return AIJSONResponse{}, fmt.Errorf("unmarshal openRouterResp error: %w\nResponse Body: %s", err, body)
    }

    if len(openRouterResp.Choices) == 0 {
        return AIJSONResponse{}, fmt.Errorf("no choices in response: %s", body)
    }

    // 8. The model response is assumed to be JSON matching AIJSONResponse.
    //    If your model is returning plain text, you'll need a different approach.
    var aiResp AIJSONResponse
    content := openRouterResp.Choices[0].Message.Content

    // Strip markdown code block formatting if present
    content = strings.TrimPrefix(content, "```json\n")
    content = strings.TrimPrefix(content, "```\n")
    content = strings.TrimSuffix(content, "\n```")
    content = strings.TrimSpace(content)

    if err := json.Unmarshal([]byte(content), &aiResp); err != nil {
        return AIJSONResponse{}, fmt.Errorf("unmarshal AIJSONResponse error: %w\nMessage Content: %s", err, content)
    }

    // 9. Add assistant message to history
    assistantMsg := fmt.Sprintf("%s (Principle: %s, Danger: %v, StatusChanged: %v)",
        aiResp.Text, aiResp.Principle, aiResp.Danger, aiResp.StatusChanged)
    c.AddMessageToHistory(Message{Role: "assistant", Content: assistantMsg})

    // 10. Return the structured response
    return aiResp, nil
}
