package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const completedEvent = `{"type":"response.completed","response":{"usage":{"input_tokens":1200,"output_tokens":80,"input_tokens_details":{"cached_tokens":1024}}}}`

func sse(w io.Writer, events ...string) {
	for _, e := range events {
		io.WriteString(w, "event: x\ndata: "+e+"\n\n")
	}
}

func testResponses(t *testing.T, tok oauthToken, h http.HandlerFunc) (*ResponsesClient, *fakeIssuer) {
	t.Helper()
	issuer := newFakeIssuer(t)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &ResponsesClient{
		url: srv.URL, model: "gpt-test", effort: "medium", session: "sess",
		tokens: testTokenSource(t, issuer, tok), http: srv.Client(), backoff: time.Millisecond,
	}, issuer
}

func validToken() oauthToken {
	return oauthToken{Access: "access-0", Refresh: "refresh-0", Expires: time.Now().Add(refreshEarly + time.Hour), AccountID: "acct"}
}

func TestResponsesRequestShape(t *testing.T) {
	var body map[string]any
	var header http.Header
	c, _ := testResponses(t, validToken(), func(w http.ResponseWriter, r *http.Request) {
		header = r.Header
		json.NewDecoder(r.Body).Decode(&body)
		sse(w, completedEvent)
	})

	msgs := []ChatMessage{
		{Role: "system", Content: "RULES"},
		{Role: "user", Content: "first"},
		{
			Role: "assistant", Content: "sending",
			ReasoningContent: `[{"type":"reasoning","summary":[],"encrypted_content":"ENC"}]`,
			ToolCalls:        []ToolCall{{ID: "call_1", Type: "function", Function: FunctionCall{Name: toolSendAlert, Arguments: `{"danger":true}`}}},
		},
		{Role: "tool", ToolCallID: "call_1", Content: "Отправлено."},
		{Role: "assistant", Content: "plain", ReasoningContent: "reasoning text of another provider"},
		{Role: "user", Content: "second"},
	}
	if _, _, err := c.Chat(context.Background(), msgs, toolDefs([]string{"src"})); err != nil {
		t.Fatal(err)
	}

	if header.Get("Authorization") != "Bearer access-0" || header.Get("ChatGPT-Account-Id") != "acct" ||
		header.Get("originator") != clientName || header.Get("session-id") != "sess" {
		t.Fatalf("headers = %v", header)
	}
	if body["model"] != "gpt-test" || body["instructions"] != "RULES" || body["store"] != false || body["stream"] != true ||
		body["prompt_cache_key"] != "sess" || body["reasoning"].(map[string]any)["effort"] != "medium" ||
		body["include"].([]any)[0] != "reasoning.encrypted_content" {
		t.Fatalf("body = %v", body)
	}
	tool := body["tools"].([]any)[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != toolSendAlert || tool["strict"] != false || tool["parameters"] == nil {
		t.Fatalf("tool = %v", tool)
	}

	var kinds []string
	for _, raw := range body["input"].([]any) {
		item := raw.(map[string]any)
		if _, ok := item["id"]; ok {
			t.Fatalf("input items must not carry ids: %v", item)
		}
		kind, _ := item["type"].(string)
		if role, ok := item["role"].(string); ok {
			kind = role + ":" + item["content"].([]any)[0].(map[string]any)["type"].(string)
		}
		kinds = append(kinds, kind)
	}
	want := "user:input_text reasoning assistant:output_text function_call function_call_output assistant:output_text user:input_text"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("input items = %s\nwant          %s", got, want)
	}
}

func TestResponsesStreamParsing(t *testing.T) {
	c, _ := testResponses(t, validToken(), func(w http.ResponseWriter, _ *http.Request) {
		sse(w,
			`{"type":"response.created"}`,
			`{"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"ENC"}}`,
			`{"type":"response.output_text.delta","delta":"ignored"}`,
			`{"type":"response.output_item.done","item":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Угроза Аркадии."}]}}`,
			`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_9","name":"send_alert","arguments":"{\"danger\":true}"}}`,
			completedEvent,
		)
	})

	msg, usage, err := c.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "x"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Role != "assistant" || msg.Content != "Угроза Аркадии." || len(msg.ToolCalls) != 1 {
		t.Fatalf("msg = %+v", msg)
	}
	if tc := msg.ToolCalls[0]; tc.ID != "call_9" || tc.Function.Name != "send_alert" || tc.Function.Arguments != `{"danger":true}` {
		t.Fatalf("tool call = %+v", tc)
	}
	if msg.ReasoningContent != `[{"type":"reasoning","summary":[],"encrypted_content":"ENC"}]` {
		t.Fatalf("reasoning must be kept without its id: %s", msg.ReasoningContent)
	}
	if usage != (Usage{Prompt: 1200, Cached: 1024, Completion: 80}) {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestResponsesErrorsAndRetries(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		events    []string
		wantCalls int32
	}{
		{"server error", http.StatusBadGateway, nil, llmAttempts},
		{"rate limit", http.StatusTooManyRequests, nil, llmAttempts},
		{"bad request", http.StatusBadRequest, nil, 1},
		{"stream cut short", http.StatusOK, []string{`{"type":"response.created"}`}, llmAttempts},
		{"overloaded", http.StatusOK, []string{`{"type":"response.failed","response":{"error":{"code":"server_is_overloaded","message":"busy"}}}`}, llmAttempts},
		{"error frame", http.StatusOK, []string{`{"type":"error","code":"server_error","message":"oops"}`}, llmAttempts},
		{"nested error frame", http.StatusOK, []string{`{"type":"error","error":{"code":"usage_not_included","message":"upgrade"}}`}, 1},
		{"context overflow", http.StatusOK, []string{`{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"too long"}}}`}, 1},
		{"incomplete", http.StatusOK, []string{`{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"}}}`}, 1},
	}
	for _, tc := range cases {
		var calls atomic.Int32
		c, _ := testResponses(t, validToken(), func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(tc.status)
			sse(w, tc.events...)
		})
		if _, _, err := c.Chat(context.Background(), nil, nil); err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
		if calls.Load() != tc.wantCalls {
			t.Errorf("%s: calls = %d, want %d", tc.name, calls.Load(), tc.wantCalls)
		}
	}
}

func TestResponsesRefreshesTokenOnUnauthorized(t *testing.T) {
	var seen []string
	c, issuer := testResponses(t, validToken(), func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		if len(seen) == 1 {
			http.Error(w, `{"detail":"token expired"}`, http.StatusUnauthorized)
			return
		}
		sse(w, completedEvent)
	})

	if _, _, err := c.Chat(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if issuer.calls.Load() != 1 || len(seen) != 2 || seen[1] != "Bearer access-1" {
		t.Fatalf("a rejected token must be refreshed and the call repeated: refreshes = %d, auth = %v", issuer.calls.Load(), seen)
	}
}

func TestResponsesFailsWithoutLogin(t *testing.T) {
	var calls atomic.Int32
	expired := oauthToken{Access: "stale", Refresh: "dead", Expires: time.Now().Add(-time.Hour)}
	c, issuer := testResponses(t, expired, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	issuer.status = http.StatusUnauthorized

	if _, _, err := c.Chat(context.Background(), nil, nil); err == nil || !strings.Contains(err.Error(), "login") {
		t.Fatalf("err = %v, want a hint to sign in again", err)
	}
	if calls.Load() != 0 {
		t.Fatal("no model call may be made without a token")
	}
}
