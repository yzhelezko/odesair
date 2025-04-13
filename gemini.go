package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
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
	// Note: Adjust the model name as needed (e.g., "gemini-1.5-flash-latest", "gemini-1.5-pro-latest")
	// See https://ai.google.dev/gemini-api/docs/models/gemini
	model := "gemini-2.5-pro-exp-03-25"
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", model, c.APIKey)

	// Construct Gemini API request payload
	// Gemini API expects alternating user/model roles.
	// We'll need to potentially adapt our history format or filter it.
	// For simplicity, let's combine the system message and the user message for now.
	// A more robust implementation would handle history conversion correctly.

	// Combine system message and user message into the prompt structure
	// Note: The Gemini API structure differs from Claude/OpenAI.
	// It uses a 'contents' array with 'parts'.
	// Let's build the contents array from our history + system message.

	// Start with the system message if present
	var contents []map[string]interface{}
	var userText string
	if c.SystemMessage != "" {
		userText = c.SystemMessage + "\n\n" + message // Combine system and first user message
	} else {
		userText = message
	}

	contents = append(contents, map[string]interface{}{
		"role": "user",
		"parts": []map[string]string{
			{"text": userText},
		},
	})

	// Add historical messages (Needs careful adaptation for Gemini's alternating roles)
	// This basic implementation sends only the system prompt + current message.
	// A full implementation would need to map our history to Gemini's format.
	// log.Printf("Current History for Gemini (simplified): %v", contents)

	// Generation Config (Optional - customize as needed)
	generationConfig := map[string]interface{}{
		"responseMimeType": "application/json", // Request JSON output
		// "temperature": 0.7,
		// "topP": 1.0,
		// "topK": 40,
		// "maxOutputTokens": 2048,
	}

	reqBodyMap := map[string]interface{}{
		"contents":         contents,
		"generationConfig": generationConfig,
		// Safety settings can be added here if needed
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

		var aiResp AIJSONResponse
		// The response text itself should be the JSON string we expect
		if err := json.Unmarshal([]byte(responseText), &aiResp); err != nil {
			log.Printf("Failed to unmarshal inner JSON from Gemini response: %v. Response text: %s", err, responseText)
			// Fallback or error handling if the inner text isn't the expected JSON
			return AIJSONResponse{}, fmt.Errorf("failed to unmarshal inner JSON from gemini response: %w. Content was: %s", err, responseText)
		}

		// Add the successful AI response to history (as 'model')
		c.AddMessageToHistory(Message{Role: "model", Content: responseText}) // Store the JSON string

		return aiResp, nil
	}

	log.Printf("No valid content found in Gemini response: %+v", geminiResp)
	return AIJSONResponse{}, fmt.Errorf("no valid content found in gemini response")
}
