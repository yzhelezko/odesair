package main

import (
	"context"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"
)

// TestLiveLLM drives real turns against the configured provider; nothing is posted to Telegram.
func TestLiveLLM(t *testing.T) {
	if os.Getenv("LIVE_LLM") == "" {
		t.Skip("set LIVE_LLM=1 to call the real LLM")
	}
	prompt, err := newPromptFile(promptPath)
	if err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		t.Fatal(err)
	}
	llm := &recordingLLM{LLM: NewLLMClient(Config{
		LLMBaseURL: getEnv("LLM_BASE_URL", defaultLLMBaseURL),
		LLMModel:   getEnv("LLM_MODEL", defaultLLMModel),
		LLMKey:     getEnv("API_KEY", ""),
		LLMEffort:  getEnv("LLM_EFFORT", defaultLLMEffort),
	})}
	tg := &fakeMessenger{}
	tools := NewToolbox(tg, []string{"odessa_infonews", "xydessa_live"}, false, loc)
	agent := NewAgent(llm, tools, prompt.Text, func() AlertStatus { return AlertActive }, loc, 32000)

	steps := []struct {
		channel, text string
		wantAlerts    int
	}{
		{"xydessa_live", "Доброе утро! Сегодня в Одессе солнечно, +18.", 0},
		{"odessa_infonews", "Шахед с моря курсом на Аркадию! Жителям Аркадии — в укрытие.", 1},
		{"xydessa_live", "Подписывайтесь на наш канал, розыгрыш призов среди подписчиков.", 1},
		{systemChannel, alertEnded, 2},
	}
	for i, s := range steps {
		start := time.Now()
		if !agent.runTurn(context.Background(), []Post{{ID: i + 1, At: time.Now(), Channel: s.channel, Text: s.text}}) {
			t.Fatalf("turn %d: llm call failed", i)
		}
		t.Logf("turn %d: %v, calls so far %d, usage %+v, sent %q",
			i, time.Since(start).Round(time.Millisecond), len(llm.requests), llm.usage, tg.sent)
		if len(tg.sent) != s.wantAlerts {
			t.Errorf("turn %d (%q): alerts = %d, want %d", i, s.text, len(tg.sent), s.wantAlerts)
		}
	}

	for n := 1; n < len(llm.requests); n++ {
		prev, cur := llm.requests[n-1], llm.requests[n]
		if len(cur) <= len(prev) || !reflect.DeepEqual(prev, cur[:len(prev)]) {
			t.Errorf("request %d does not extend request %d: the prompt cache cannot hit", n, n-1)
		}
	}
	if llm.usage.Cached == 0 {
		t.Log("provider reported no cached tokens on the last call")
	}
}

type recordingLLM struct {
	LLM
	requests [][]ChatMessage
	usage    Usage
}

func (r *recordingLLM) Chat(ctx context.Context, msgs []ChatMessage, tools []ToolDef) (ChatMessage, Usage, error) {
	r.requests = append(r.requests, slices.Clone(msgs))
	msg, usage, err := r.LLM.Chat(ctx, msgs, tools)
	r.usage = usage
	return msg, usage, err
}
