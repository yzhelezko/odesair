package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

const (
	llmTimeout  = 45 * time.Second
	llmAttempts = 3
	llmBackoff  = 500 * time.Millisecond
)

type ChatMessage struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolDef struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type chatRequest struct {
	Model           string        `json:"model"`
	Messages        []ChatMessage `json:"messages"`
	Tools           []ToolDef     `json:"tools,omitempty"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message ChatMessage `json:"message"`
	} `json:"choices"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
	Usage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

type LLMClient struct {
	url     string
	model   string
	key     string
	effort  string
	http    *http.Client
	backoff time.Duration
}

func NewLLMClient(cfg Config) *LLMClient {
	return &LLMClient{
		url:     cfg.LLMBaseURL + "/chat/completions",
		model:   cfg.LLMModel,
		key:     cfg.LLMKey,
		effort:  cfg.LLMEffort,
		http:    &http.Client{Timeout: llmTimeout},
		backoff: llmBackoff,
	}
}

func (c *LLMClient) Chat(ctx context.Context, msgs []ChatMessage, tools []ToolDef) (ChatMessage, Usage, error) {
	body, err := json.Marshal(chatRequest{Model: c.model, Messages: msgs, Tools: tools, ReasoningEffort: c.effort})
	if err != nil {
		return ChatMessage{}, Usage{}, fmt.Errorf("llm encode: %w", err)
	}

	for attempt := 0; ; attempt++ {
		msg, usage, retry, err := c.do(ctx, body)
		if err == nil {
			return msg, usage, nil
		}
		if !retry || attempt == llmAttempts-1 {
			return ChatMessage{}, Usage{}, err
		}
		slog.Warn("llm retry", "attempt", attempt+1, "err", err)
		select {
		case <-ctx.Done():
			return ChatMessage{}, Usage{}, ctx.Err()
		case <-time.After(c.backoff << attempt):
		}
	}
}

func (c *LLMClient) do(ctx context.Context, body []byte) (msg ChatMessage, usage Usage, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return msg, usage, false, fmt.Errorf("llm request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.key)

	resp, err := c.http.Do(req)
	if err != nil {
		return msg, usage, ctx.Err() == nil, fmt.Errorf("llm request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return msg, usage, true, fmt.Errorf("llm read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		retry = resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return msg, usage, retry, fmt.Errorf("llm status %d: %s", resp.StatusCode, snippet(raw))
	}

	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return msg, usage, true, fmt.Errorf("llm decode: %w: %s", err, snippet(raw))
	}
	if out.Error.Message != "" {
		return msg, usage, true, fmt.Errorf("llm error: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return msg, usage, true, errors.New("llm: no choices in response")
	}

	msg = out.Choices[0].Message
	msg.Role = "assistant"
	for i := range msg.ToolCalls {
		tc := &msg.ToolCalls[i]
		tc.Type = "function"
		if tc.ID == "" {
			tc.ID = "call_" + strconv.Itoa(i)
		}
	}
	usage = Usage{
		Prompt:     out.Usage.PromptTokens,
		Cached:     out.Usage.PromptTokensDetails.CachedTokens,
		Completion: out.Usage.CompletionTokens,
	}
	return msg, usage, false, nil
}

func snippet(b []byte) string {
	const max = 300
	if len(b) > max {
		return string(b[:max]) + "…"
	}
	return string(b)
}
