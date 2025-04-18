package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"time"
)

// AddMessageToHistory adds a message to the client's history, maintaining max history size.
func (c *GeminiClient) AddMessageToHistory(message Message) {
	c.MessageHistory = append(c.MessageHistory, message)
	if len(c.MessageHistory) > maxMessageHistory {
		c.MessageHistory = c.MessageHistory[1:] // Remove the oldest message
	}
}

// GetMessageHistory returns the current message history.
func (c *GeminiClient) GetMessageHistory() []Message {
	return c.MessageHistory
}

// SendMessage sends the current message history to the Gemini API and returns the AI's response.
func (c *GeminiClient) SendMessage(ctx context.Context, message string) (AIJSONResponse, error) {
	// Add user message to history at the beginning
	c.AddMessageToHistory(Message{Role: "user", Content: message})

	// Note: Adjust the model name as needed (e.g., "gemini-1.5-flash-latest", "gemini-1.5-pro-latest")
	// See https://ai.google.dev/gemini-api/docs/models/gemini
	// model := "gemini-2.5-pro-exp-03-25"
	model := "gemini-2.5-flash-preview-04-17"
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", model, c.APIKey)

	// Construct Gemini API request payload
	// Gemini API expects alternating user/model roles

	var contents []map[string]interface{}

	// Start with system message if present
	if c.SystemMessage != "" {
		contents = append(contents, map[string]interface{}{
			"role": "user",
			"parts": []map[string]string{
				{"text": c.SystemMessage + "\n Текущее время: " + time.Now().Format("15:04:05")},
			},
		})
	}

	// Add historical messages
	for _, msg := range c.MessageHistory {
		// Map our roles to Gemini's roles (user and model)
		role := msg.Role
		if role == "assistant" {
			role = "model"
		}

		contents = append(contents, map[string]interface{}{
			"role": role,
			"parts": []map[string]string{
				{"text": msg.Content},
			},
		})
	}

	log.Printf("Sending message history to Gemini with %d messages", len(contents))

	// Configure thinking budget (value between 0-24576)
	// 0 = disabled, 1-1024 will be set to 1024
	thinkingConfig := map[string]interface{}{
		"thinkingBudget": 2048, // Default thinking budget
	}

	// Main request configuration
	generationConfig := map[string]interface{}{
		"thinkingConfig": thinkingConfig,
		// Other config options can go here
		// "temperature": 0.7,
		// "topP": 1.0,
		// "topK": 40,
		// "maxOutputTokens": 2048,
	}

	reqBodyMap := map[string]interface{}{
		"contents":         contents,
		"generationConfig": generationConfig,
	}

	reqBody, err := json.Marshal(reqBodyMap)
	if err != nil {
		return AIJSONResponse{}, fmt.Errorf("failed to marshal gemini request body: %w", err)
	}

	log.Printf("Gemini Request Body: %s", string(reqBody)) // Log the request body for debugging

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return AIJSONResponse{}, fmt.Errorf("failed to create gemini request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return AIJSONResponse{}, fmt.Errorf("failed to send request to gemini: %w", err)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return AIJSONResponse{}, fmt.Errorf("failed to read gemini response body: %w", err)
	}

	log.Printf("Gemini Raw Response: %s", string(body)) // Log raw response

	if resp.StatusCode != http.StatusOK {
		return AIJSONResponse{}, fmt.Errorf("gemini API request failed with status %d: %s", resp.StatusCode, string(body))
	}

	// Parse the Gemini response structure
	var geminiResp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
				Role string `json:"role"`
			} `json:"content"`
		} `json:"candidates"`
		// PromptFeedback can be checked for safety blocks
		PromptFeedback *struct {
			BlockReason string `json:"blockReason"`
			// SafetyRatings can also be included
		} `json:"promptFeedback"`
	}

	if err := json.Unmarshal(body, &geminiResp); err != nil {
		return AIJSONResponse{}, fmt.Errorf("failed to unmarshal gemini response: %w body: %s", err, string(body))
	}

	// Check for prompt feedback indicating blockage
	if geminiResp.PromptFeedback != nil && geminiResp.PromptFeedback.BlockReason != "" {
		log.Printf("Gemini request blocked, reason: %s", geminiResp.PromptFeedback.BlockReason)
		return AIJSONResponse{}, fmt.Errorf("gemini request blocked due to safety settings: %s", geminiResp.PromptFeedback.BlockReason)
	}

	// Extract the text content and attempt to unmarshal it into our AIJSONResponse
	if len(geminiResp.Candidates) > 0 && len(geminiResp.Candidates[0].Content.Parts) > 0 {
		responseText := geminiResp.Candidates[0].Content.Parts[0].Text
		log.Printf("Gemini Response Text (before JSON parse): %s", responseText) // Log the text part

		// Clean and parse the content
		responseText = strings.TrimSpace(responseText)
		responseText = strings.TrimPrefix(responseText, "```json")
		responseText = strings.TrimPrefix(responseText, "```yaml")
		responseText = strings.TrimPrefix(responseText, "```")
		responseText = strings.TrimSuffix(responseText, "```")
		responseText = strings.TrimSpace(responseText)

		var aiResp AIJSONResponse
		// The response text itself should be the JSON string we expect
		if err := json.Unmarshal([]byte(responseText), &aiResp); err != nil {
			log.Printf("Failed to unmarshal inner JSON from Gemini response: %v. Response text: %s", err, responseText)
			// Fallback or error handling if the inner text isn't the expected JSON
			return AIJSONResponse{}, fmt.Errorf("failed to unmarshal inner JSON from gemini response: %w. Content was: %s", err, responseText)
		}

		// Add the successful AI response to history (as 'model') - matching ChatGPT implementation format
		c.AddMessageToHistory(Message{Role: "assistant", Content: fmt.Sprintf("%s Danger: %v StatusChanged: %v", aiResp.Text, aiResp.Danger, aiResp.StatusChanged)})

		return aiResp, nil
	}

	log.Printf("No valid content found in Gemini response: %+v", geminiResp)
	return AIJSONResponse{}, fmt.Errorf("no valid content found in gemini response")
}
