package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

const openaiResponsesURL = "https://chatgpt.com/backend-api/codex/responses"

// ResponsesClient speaks the OpenAI Responses API of the ChatGPT backend,
// authorised by a ChatGPT sign-in instead of an API key.
type ResponsesClient struct {
	url     string
	model   string
	effort  string
	session string
	tokens  *tokenSource
	http    *http.Client
	backoff time.Duration
}

func NewResponsesClient(cfg LLMConfig) *ResponsesClient {
	return &ResponsesClient{
		url:     openaiResponsesURL,
		model:   cfg.Model,
		effort:  cfg.Effort,
		session: randomString(),
		tokens:  newTokenSource(cfg.AuthFile, cfg.AuthSecret),
		http:    &http.Client{Timeout: llmTimeout},
		backoff: llmBackoff,
	}
}

// Notice is appended to outgoing alerts while it is non-empty.
func (c *ResponsesClient) Notice() string { return c.tokens.Warning() }

func (c *ResponsesClient) Check(ctx context.Context) error { return c.tokens.Check(ctx) }

type responsesRequest struct {
	Model          string          `json:"model"`
	Instructions   string          `json:"instructions"`
	Input          []any           `json:"input"`
	Tools          []responsesTool `json:"tools,omitempty"`
	Store          bool            `json:"store"`
	Stream         bool            `json:"stream"`
	Include        []string        `json:"include"`
	PromptCacheKey string          `json:"prompt_cache_key"`
	Reasoning      *reasoningOpt   `json:"reasoning,omitempty"`
}

type reasoningOpt struct {
	Effort string `json:"effort"`
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

type inputMessage struct {
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}

type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type functionCall struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type functionOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// reasoningItem is the model's encrypted reasoning. With store=false it has to
// be sent back, without its id, for the next request to continue from it.
type reasoningItem struct {
	Type             string          `json:"type"`
	Summary          json.RawMessage `json:"summary"`
	EncryptedContent string          `json:"encrypted_content"`
}

type streamEvent struct {
	Type string `json:"type"`
	Item struct {
		Type             string          `json:"type"`
		CallID           string          `json:"call_id"`
		Name             string          `json:"name"`
		Arguments        string          `json:"arguments"`
		Content          []contentPart   `json:"content"`
		Summary          json.RawMessage `json:"summary"`
		EncryptedContent string          `json:"encrypted_content"`
	} `json:"item"`
	Response struct {
		Usage struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
		Error             streamError `json:"error"`
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	} `json:"response"`
	Error   streamError `json:"error"`
	Message string      `json:"message"`
	Code    any         `json:"code"`
}

type streamError struct {
	Message string `json:"message"`
	Code    any    `json:"code"`
}

// Requests the backend rejects for good; anything else is worth another attempt.
var fatalCodes = []string{"context_length_exceeded", "insufficient_quota", "usage_not_included", "invalid_prompt"}

func (c *ResponsesClient) Chat(ctx context.Context, msgs []ChatMessage, tools []ToolDef) (msg ChatMessage, usage Usage, err error) {
	req := responsesRequest{
		Model:          c.model,
		Input:          []any{},
		Stream:         true,
		Include:        []string{"reasoning.encrypted_content"},
		PromptCacheKey: c.session,
	}
	if c.effort != "" {
		req.Reasoning = &reasoningOpt{Effort: c.effort}
	}
	for _, t := range tools {
		req.Tools = append(req.Tools, responsesTool{
			Type: "function", Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters,
		})
	}
	for _, m := range msgs {
		switch m.Role {
		case "system":
			req.Instructions = m.Content
		case "user":
			req.Input = append(req.Input, inputMessage{Role: "user", Content: []contentPart{{Type: "input_text", Text: m.Content}}})
		case "tool":
			req.Input = append(req.Input, functionOutput{Type: "function_call_output", CallID: m.ToolCallID, Output: m.Content})
		case "assistant":
			var reasoning []reasoningItem
			if json.Unmarshal([]byte(m.ReasoningContent), &reasoning) == nil {
				for _, r := range reasoning {
					req.Input = append(req.Input, r)
				}
			}
			if m.Content != "" {
				req.Input = append(req.Input, inputMessage{Role: "assistant", Content: []contentPart{{Type: "output_text", Text: m.Content}}})
			}
			for _, tc := range m.ToolCalls {
				req.Input = append(req.Input, functionCall{Type: "function_call", CallID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
			}
		}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return msg, usage, fmt.Errorf("llm encode: %w", err)
	}
	err = retry(ctx, c.backoff, func() (again bool, err error) {
		msg, usage, again, err = c.do(ctx, body)
		return again, err
	})
	return msg, usage, err
}

func (c *ResponsesClient) do(ctx context.Context, body []byte) (msg ChatMessage, usage Usage, retry bool, err error) {
	tok, err := c.tokens.Token(ctx)
	if err != nil {
		return msg, usage, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return msg, usage, false, fmt.Errorf("llm request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+tok.Access)
	req.Header.Set("originator", clientName)
	req.Header.Set("User-Agent", clientName)
	req.Header.Set("session-id", c.session)
	if tok.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", tok.AccountID)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return msg, usage, ctx.Err() == nil, fmt.Errorf("llm request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == http.StatusUnauthorized {
			c.tokens.Expire()
		}
		retry = resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return msg, usage, retry, fmt.Errorf("llm status %d: %s", resp.StatusCode, snippet(raw))
	}
	return readResponseStream(resp.Body)
}

func readResponseStream(r io.Reader) (msg ChatMessage, usage Usage, retry bool, err error) {
	var (
		text      strings.Builder
		reasoning []reasoningItem
		br        = bufio.NewReaderSize(r, 64<<10)
	)
	msg.Role = "assistant"
	for {
		line, readErr := br.ReadBytes('\n')
		if data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:")); ok {
			var ev streamEvent
			if json.Unmarshal(data, &ev) != nil {
				continue
			}
			switch ev.Type {
			case "response.output_item.done":
				switch ev.Item.Type {
				case "message":
					for _, part := range ev.Item.Content {
						if part.Type == "output_text" {
							text.WriteString(part.Text)
						}
					}
				case "function_call":
					msg.ToolCalls = append(msg.ToolCalls, ToolCall{
						ID: ev.Item.CallID, Type: "function",
						Function: FunctionCall{Name: ev.Item.Name, Arguments: ev.Item.Arguments},
					})
				case "reasoning":
					if ev.Item.EncryptedContent != "" {
						summary := ev.Item.Summary
						if len(summary) == 0 {
							summary = json.RawMessage("[]")
						}
						reasoning = append(reasoning, reasoningItem{Type: "reasoning", Summary: summary, EncryptedContent: ev.Item.EncryptedContent})
					}
				}
			case "response.completed":
				msg.Content = text.String()
				if len(reasoning) > 0 {
					raw, _ := json.Marshal(reasoning)
					msg.ReasoningContent = string(raw)
				}
				u := ev.Response.Usage
				return msg, Usage{Prompt: u.InputTokens, Cached: u.InputTokensDetails.CachedTokens, Completion: u.OutputTokens}, false, nil
			case "response.incomplete":
				return msg, usage, false, fmt.Errorf("llm response incomplete: %s", ev.Response.IncompleteDetails.Reason)
			case "response.failed":
				return streamFailure(ev.Response.Error)
			case "error":
				if ev.Error.Message != "" {
					return streamFailure(ev.Error)
				}
				return streamFailure(streamError{Message: ev.Message, Code: ev.Code})
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				readErr = errors.New("stream ended before the response completed")
			}
			return msg, usage, true, fmt.Errorf("llm stream: %w", readErr)
		}
	}
}

func streamFailure(e streamError) (ChatMessage, Usage, bool, error) {
	code := fmt.Sprint(e.Code)
	return ChatMessage{}, Usage{}, !slices.Contains(fatalCodes, code), fmt.Errorf("llm error %s: %s", code, e.Message)
}
