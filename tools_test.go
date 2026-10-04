package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func sendCall(args string) ToolCall {
	return ToolCall{ID: "c", Function: FunctionCall{Name: toolSendAlert, Arguments: args}}
}

func alertArgs(danger bool, text string) string {
	return fmt.Sprintf(`{"reason":"r","danger":%v,"text":%q}`, danger, text)
}

func TestSendAlertFormatsByDanger(t *testing.T) {
	tg := &fakeMessenger{}
	tb := NewToolbox(tg, []string{"src"}, false, time.UTC)

	if res, again := tb.Call(context.Background(), sendCall(alertArgs(true, "🚨 ракета на центр"))); again {
		t.Fatalf("unexpected rejection: %s", res)
	}
	if res, again := tb.Call(context.Background(), sendCall(alertArgs(false, "отбой"))); again {
		t.Fatalf("unexpected rejection: %s", res)
	}
	want := []string{"🚨 ракета на центр", "✅ отбой"}
	if len(tg.sent) != 2 || tg.sent[0] != want[0] || tg.sent[1] != want[1] {
		t.Fatalf("sent = %q, want %q", tg.sent, want)
	}
	if tg.silent[0] || !tg.silent[1] {
		t.Fatalf("silent = %v, want danger loud and clear silent", tg.silent)
	}
	if tb.Sends() != 2 {
		t.Fatalf("sends = %d", tb.Sends())
	}
}

func TestSendAlertRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"malformed":     `{"text":`,
		"no danger":     `{"reason":"r","text":"x"}`,
		"empty text":    alertArgs(true, " 🚨 "),
		"too long":      alertArgs(true, strings.Repeat("я", maxAlertRunes+1)),
		"unknown tool:": "",
	}
	for name, args := range cases {
		tg := &fakeMessenger{}
		tb := NewToolbox(tg, []string{"src"}, false, time.UTC)
		tc := sendCall(args)
		if strings.HasSuffix(name, ":") {
			tc.Function.Name = "delete_channel"
		}
		res, again := tb.Call(context.Background(), tc)
		if !again || !strings.HasPrefix(res, "Ошибка:") || len(tg.sent) != 0 || tb.Sends() != 0 {
			t.Errorf("%s: res = %q, again = %v, sent = %d", name, res, again, len(tg.sent))
		}
	}
}

func TestSendAlertDedupAndRateLimit(t *testing.T) {
	tg := &fakeMessenger{}
	tb := NewToolbox(tg, []string{"src"}, false, time.UTC)
	clock := time.Now()
	tb.now = func() time.Time { return clock }
	ctx := context.Background()

	tb.Call(ctx, sendCall(alertArgs(true, "угроза")))
	if res, again := tb.Call(ctx, sendCall(alertArgs(true, "угроза"))); !again || !strings.Contains(res, "дубликат") {
		t.Fatalf("duplicate must be rejected, got %q", res)
	}
	clock = clock.Add(dedupWindow)
	if _, again := tb.Call(ctx, sendCall(alertArgs(true, "угроза"))); again {
		t.Fatal("same text after the dedup window must pass")
	}

	clock = clock.Add(time.Hour)
	for i := range rateLimit {
		if res, again := tb.Call(ctx, sendCall(alertArgs(true, fmt.Sprint("угроза ", i)))); again {
			t.Fatalf("alert %d rejected: %s", i, res)
		}
	}
	if res, again := tb.Call(ctx, sendCall(alertArgs(true, "ещё одна"))); !again || !strings.Contains(res, "лимит") {
		t.Fatalf("rate limit must reject, got %q", res)
	}
	clock = clock.Add(rateWindow)
	if _, again := tb.Call(ctx, sendCall(alertArgs(true, "ещё одна"))); again {
		t.Fatal("rate limit must lift after the window")
	}
}

func TestSendAlertDryRunAndTelegramFailure(t *testing.T) {
	tg := &fakeMessenger{}
	tb := NewToolbox(tg, []string{"src"}, true, time.UTC)
	if _, again := tb.Call(context.Background(), sendCall(alertArgs(true, "тест"))); again {
		t.Fatal("dry run must report success")
	}
	if len(tg.sent) != 0 || tb.Sends() != 1 || len(tb.Alerts()) != 1 {
		t.Fatalf("dry run: sent = %d, sends = %d, alerts = %d", len(tg.sent), tb.Sends(), len(tb.Alerts()))
	}

	tg = &fakeMessenger{err: errors.New("FLOOD_WAIT")}
	tb = NewToolbox(tg, []string{"src"}, false, time.UTC)
	res, again := tb.Call(context.Background(), sendCall(alertArgs(true, "тест")))
	if !again || !strings.Contains(res, "FLOOD_WAIT") || tb.Sends() != 0 || len(tb.Alerts()) != 0 {
		t.Fatalf("failed send must not be recorded: %q", res)
	}
}

func TestAlertMemorySeedAndExpiry(t *testing.T) {
	tb := NewToolbox(&fakeMessenger{}, []string{"src"}, false, time.UTC)
	clock := time.Now()
	tb.now = func() time.Time { return clock }

	tb.Seed([]Post{
		{At: clock.Add(-alertMemory - time.Minute), Text: "🚨 старое"},
		{At: clock.Add(-time.Hour), Text: "🚨 шахеды"},
		{At: clock.Add(-time.Minute), Text: "✅ отбой"},
		{At: clock, Text: "просто пост"},
	})
	alerts := tb.Alerts()
	if len(alerts) != 2 || !alerts[0].danger || alerts[0].text != "шахеды" || alerts[1].danger || alerts[1].text != "отбой" {
		t.Fatalf("alerts = %+v", alerts)
	}
}

func TestGetRecentMessages(t *testing.T) {
	now := time.Now()
	tg := &fakeMessenger{posts: []Post{
		{ID: 1, At: now.Add(-recentWindow - time.Minute), Text: "давно"},
		{ID: 2, At: now.Add(-time.Minute), Text: "  "},
		{ID: 3, At: now, Text: "свежее"},
	}}
	tb := NewToolbox(tg, []string{"src"}, false, time.UTC)
	call := func(args string) (string, bool) {
		return tb.Call(context.Background(), ToolCall{Function: FunctionCall{Name: toolGetRecent, Arguments: args}})
	}

	res, again := call(`{"channel":"src","limit":500}`)
	if !again || !strings.Contains(res, "src:\nсвежее") || strings.Contains(res, "давно") {
		t.Fatalf("res = %q", res)
	}
	if res, _ := call(`{"channel":"other"}`); !strings.Contains(res, "неизвестный канал") {
		t.Fatalf("res = %q", res)
	}
	tg.posts = nil
	if res, _ := call(`{"channel":"src"}`); !strings.Contains(res, "Нет сообщений") {
		t.Fatalf("res = %q", res)
	}
}
