package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// GLMClient implements the AIClient interface for Z.AI's GLM model.
// Documentation: https://docs.z.ai/guides/develop/http/introduction
// Migration guide: https://docs.z.ai/guides/overview/migrate-to-glm-new
type GLMClient struct {
	APIKey         string
	HTTPClient     *http.Client
	SystemMessage  string
	MessageHistory []Message
	// UseCodingPlan indicates whether to use the GLM Coding Plan endpoint
	UseCodingPlan bool
}

// GLM API configuration
const (
	// GLM general API endpoint
	glmAPIEndpoint = "https://api.z.ai/api/paas/v4/chat/completions"
	// GLM Coding Plan endpoint (for subscribers)
	glmCodingAPIEndpoint = "https://api.z.ai/api/coding/paas/v4/chat/completions"
	// Model identifier
	glmModel = "glm-5"
)

// AddMessageToHistory adds a message to the client's history, maintaining max history size.
func (c *GLMClient) AddMessageToHistory(message Message) {
	c.MessageHistory = append(c.MessageHistory, message)
	if len(c.MessageHistory) > maxMessageHistory {
		c.MessageHistory = c.MessageHistory[1:] // Remove the oldest message
	}
}

// GetMessageHistory returns the current message history.
func (c *GLMClient) GetMessageHistory() []Message {
	return c.MessageHistory
}

// SendMessage sends the current message history to the GLM API and returns the AI's response.
func (c *GLMClient) SendMessage(ctx context.Context, message Message) (AIJSONResponse, error) {
	// Add user message to history at the beginning
	c.AddMessageToHistory(message)

	// Always use the general API endpoint.
	// Note: The Coding Plan endpoint (glmCodingAPIEndpoint) is for coding tools only
	// (Claude Code, Cline, etc.), not for direct API calls.
	endpoint := glmAPIEndpoint
	if c.UseCodingPlan {
		endpoint = glmCodingAPIEndpoint
	}

	// Build messages array in OpenAI-compatible format
	var apiMessages []map[string]interface{}

	// Add system message first
	if c.SystemMessage != "" {
		apiMessages = append(apiMessages, map[string]interface{}{
			"role":    "system",
			"content": c.SystemMessage + "\n Current time: " + time.Now().Format("15:04:05"),
		})
	}

	// Add historical messages
	for _, msg := range c.MessageHistory {
		role := msg.Role
		// GLM uses "assistant" role (like OpenAI), not "model" like Gemini

		// If using Coding Plan (GLM), skip images - it's text-only
		// If not using Coding Plan, include images with vision model
		if !c.UseCodingPlan && len(msg.Images) > 0 {
			// Multimodal message with images (vision mode)
			var contentParts []map[string]interface{}

			// Add text content
			if msg.Content != "" {
				contentParts = append(contentParts, map[string]interface{}{
					"type": "text",
					"text": msg.Content,
				})
			}

			// Add images as base64-encoded data URLs
			for _, img := range msg.Images {
				contentParts = append(contentParts, map[string]interface{}{
					"type": "image_url",
					"image_url": map[string]string{
						"url": fmt.Sprintf("data:%s;base64,%s", img.MIMEType, base64.StdEncoding.EncodeToString(img.Data)),
					},
				})
			}

			apiMessages = append(apiMessages, map[string]interface{}{
				"role":    role,
				"content": contentParts,
			})
		} else {
			// Text-only message (or Coding Plan mode - skip images)
			apiMessages = append(apiMessages, map[string]interface{}{
				"role":    role,
				"content": msg.Content,
			})
		}
	}

	// Always use GLM
	// Coding Plan: text-only (images skipped above)
	// Regular API: supports images
	if c.UseCodingPlan {
		log.Printf("Sending message history to GLM (Coding Plan, text-only) with %d messages", len(apiMessages))
	} else {
		log.Printf("Sending message history to GLM with %d messages", len(apiMessages))
	}

	// Build request body
	// Reference: https://docs.z.ai/guides/overview/migrate-to-glm-new
	reqBodyMap := map[string]interface{}{
		"model":       glmModel,
		"messages":    apiMessages,
		"temperature": 1.0,  // Recommended default for GLM
		"max_tokens":  4096, // Reasonable default
		"thinking":    map[string]interface{}{"type": "enabled"},
	}

	reqBody, err := json.Marshal(reqBodyMap)
	if err != nil {
		return AIJSONResponse{}, fmt.Errorf("failed to marshal GLM request body: %w", err)
	}

	// log.Printf("GLM Request Body: %s", string(reqBody)) // Debug logging

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBuffer(reqBody))
	if err != nil {
		return AIJSONResponse{}, fmt.Errorf("failed to create GLM request: %w", err)
	}

	// Set required headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.APIKey))
	req.Header.Set("Accept-Language", "en-US,en") // Optional: for English responses

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return AIJSONResponse{}, fmt.Errorf("failed to send request to GLM: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return AIJSONResponse{}, fmt.Errorf("failed to read GLM response body: %w", err)
	}

	log.Printf("GLM Raw Response: %s", string(body)) // Log raw response

	if resp.StatusCode != http.StatusOK {
		return AIJSONResponse{}, fmt.Errorf("GLM API request failed with status %d: %s", resp.StatusCode, string(body))
	}

	// Parse the OpenAI-compatible response structure
	var glmResp struct {
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}

	// Handle UTF-8 BOM if present
	body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))

	if err := json.Unmarshal(body, &glmResp); err != nil {
		return AIJSONResponse{}, fmt.Errorf("failed to unmarshal GLM response: %w body: %s", err, string(body))
	}

	// Check for API error
	if glmResp.Error.Message != "" {
		return AIJSONResponse{}, fmt.Errorf("GLM API error: %s (type: %s, code: %s)",
			glmResp.Error.Message, glmResp.Error.Type, glmResp.Error.Code)
	}

	// Extract the response content
	if len(glmResp.Choices) == 0 {
		return AIJSONResponse{}, fmt.Errorf("no choices in GLM response")
	}

	responseText := glmResp.Choices[0].Message.Content
	log.Printf("GLM Response Text (before JSON parse): %s", responseText)

	// Log token usage
	if glmResp.Usage.TotalTokens > 0 {
		log.Printf("GLM Token Usage - Prompt: %d, Completion: %d, Total: %d",
			glmResp.Usage.PromptTokens, glmResp.Usage.CompletionTokens, glmResp.Usage.TotalTokens)
	}

	// Clean and parse the content (same as other providers)
	responseText = strings.TrimSpace(responseText)
	responseText = strings.TrimPrefix(responseText, "```json")
	responseText = strings.TrimPrefix(responseText, "```yaml")
	responseText = strings.TrimPrefix(responseText, "```")
	responseText = strings.TrimSuffix(responseText, "```")
	responseText = strings.TrimSpace(responseText)

	var aiResp AIJSONResponse
	if err := json.Unmarshal([]byte(responseText), &aiResp); err != nil {
		log.Printf("Failed to unmarshal inner JSON from GLM response: %v. Response text: %s", err, responseText)
		return AIJSONResponse{}, fmt.Errorf("failed to unmarshal inner JSON from GLM response: %w. Content was: %s", err, responseText)
	}

	// Add the successful AI response to history
	c.AddMessageToHistory(Message{
		Role:    "assistant",
		Content: fmt.Sprintf("%s Danger: %v StatusChanged: %v", aiResp.Text, aiResp.Danger, aiResp.StatusChanged),
	})

	return aiResp, nil
}
