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

func testLLM(t *testing.T, effort string, h http.HandlerFunc) *LLMClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &LLMClient{url: srv.URL, model: "m", key: "secret", effort: effort, http: srv.Client(), backoff: time.Millisecond}
}

func chat(c *LLMClient, msgs []ChatMessage, tools []ToolDef) (ChatMessage, error) {
	msg, _, err := c.Chat(context.Background(), msgs, tools)
	return msg, err
}

func TestChatRequestAndToolCallParsing(t *testing.T) {
	var got map[string]any
	c := testLLM(t, "low", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		io.WriteString(w, `{"choices":[{"message":{"content":null,"reasoning_content":"think","tool_calls":[
			{"id":"a","type":"function","function":{"name":"send_alert","arguments":"{\"danger\":true}"}},
			{"function":{"name":"get_recent_messages","arguments":"{\"channel\":\"src\"}"}}
		]}}],"usage":{"prompt_tokens":900,"completion_tokens":40,"prompt_tokens_details":{"cached_tokens":768}}}`)
	})

	msg, usage, err := c.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, toolDefs([]string{"src"}))
	if err != nil {
		t.Fatal(err)
	}
	if got["model"] != "m" || len(got["tools"].([]any)) != 2 || got["reasoning_effort"] != "low" {
		t.Fatalf("request = %v", got)
	}
	if usage != (Usage{Prompt: 900, Cached: 768, Completion: 40}) {
		t.Fatalf("usage = %+v", usage)
	}
	if msg.Role != "assistant" || msg.Content != "" || len(msg.ToolCalls) != 2 {
		t.Fatalf("msg = %+v", msg)
	}
	if a := msg.ToolCalls[0]; a.ID != "a" || a.Function.Arguments != `{"danger":true}` {
		t.Fatalf("first call = %+v", a)
	}
	if b := msg.ToolCalls[1]; b.ID != "call_1" || b.Type != "function" {
		t.Fatalf("missing id and type must be filled in: %+v", b)
	}

	// The reply goes back into history as received, reasoning included.
	raw, _ := json.Marshal(msg)
	for _, want := range []string{`"reasoning_content":"think"`, `"arguments":"{\"channel\":\"src\"}"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("re-encoded message lacks %s: %s", want, raw)
		}
	}
}

func TestChatOmitsOptionalFields(t *testing.T) {
	var got map[string]any
	c := testLLM(t, "", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	})
	if _, err := chat(c, []ChatMessage{{Role: "user", Content: "hi"}}, nil); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"reasoning_effort", "tools"} {
		if _, ok := got[key]; ok {
			t.Fatalf("%s must be omitted when unset", key)
		}
	}
}

func TestChatRetries(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantCalls int32
	}{
		{"server error", http.StatusBadGateway, "bad gateway", llmAttempts},
		{"rate limit", http.StatusTooManyRequests, "slow down", llmAttempts},
		{"error in 200", http.StatusOK, `{"error":{"message":"upstream"}}`, llmAttempts},
		{"no choices", http.StatusOK, `{"choices":[]}`, llmAttempts},
		{"client error", http.StatusUnauthorized, "bad key", 1},
	}
	for _, tc := range cases {
		var calls atomic.Int32
		c := testLLM(t, "", func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(tc.status)
			io.WriteString(w, tc.body)
		})
		if _, err := chat(c, nil, nil); err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
		if calls.Load() != tc.wantCalls {
			t.Errorf("%s: calls = %d, want %d", tc.name, calls.Load(), tc.wantCalls)
		}
	}

	var calls atomic.Int32
	c := testLLM(t, "", func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	})
	if msg, err := chat(c, nil, nil); err != nil || msg.Content != "ok" || calls.Load() != 2 {
		t.Fatalf("recovery: msg = %+v, err = %v, calls = %d", msg, err, calls.Load())
	}
}

func TestChatStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	c := testLLM(t, "", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		cancel()
		w.WriteHeader(http.StatusInternalServerError)
	})
	c.backoff = time.Hour
	if _, _, err := c.Chat(ctx, nil, nil); err == nil || calls.Load() != 1 {
		t.Fatalf("err = %v, calls = %d", err, calls.Load())
	}
}
