package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}

type fakeLLM struct {
	mu    sync.Mutex
	calls [][]ChatMessage
	fn    func(n int) (ChatMessage, error)
	usage func(msgs []ChatMessage) Usage
}

func (f *fakeLLM) Chat(_ context.Context, msgs []ChatMessage, _ []ToolDef) (ChatMessage, Usage, error) {
	f.mu.Lock()
	n := len(f.calls)
	f.calls = append(f.calls, slices.Clone(msgs))
	f.mu.Unlock()
	var u Usage
	if f.usage != nil {
		u = f.usage(msgs)
	}
	msg, err := f.fn(n)
	return msg, u, err
}

// lastUser is the newest user message of call n.
func (f *fakeLLM) lastUser(n int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range slices.Backward(f.calls[n]) {
		if m.Role == "user" {
			return m.Content
		}
	}
	return ""
}

type fakeMessenger struct {
	mu     sync.Mutex
	sent   []string
	silent []bool
	posts  []Post
	err    error
	notify chan struct{}
}

func (f *fakeMessenger) Send(_ context.Context, text string, silent bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, text)
	f.silent = append(f.silent, silent)
	if f.notify != nil {
		f.notify <- struct{}{}
	}
	return nil
}

func (f *fakeMessenger) Recent(_ context.Context, channel string, _ int) ([]Post, error) {
	posts := slices.Clone(f.posts)
	for i := range posts {
		posts[i].Channel = channel
	}
	return posts, f.err
}

func toolCall(name, args string) ChatMessage {
	return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{
		{ID: "c1", Type: "function", Function: FunctionCall{Name: name, Arguments: args}},
	}}
}

func say(s string) ChatMessage { return ChatMessage{Role: "assistant", Content: s} }

func always(msg ChatMessage) func(int) (ChatMessage, error) {
	return func(int) (ChatMessage, error) { return msg, nil }
}

const dangerArgs = `{"reason":"правило 5","danger":true,"text":"Шахеды на Аркадию"}`

func newTestAgent(llm LLM, tg Messenger) *Agent {
	tools := NewToolbox(tg, []string{"src"}, false, time.UTC)
	a := NewAgent(llm, tools, func() string { return "SYSTEM" }, func() AlertStatus { return AlertActive }, time.UTC, 32000)
	a.retryDelay = time.Millisecond
	return a
}

func post(id int, text string) Post {
	return Post{ID: id, At: time.Now(), Channel: "src", Text: text}
}

func TestTurnSendsAlertWithoutFollowUpCall(t *testing.T) {
	llm := &fakeLLM{fn: always(toolCall(toolSendAlert, dangerArgs))}
	tg := &fakeMessenger{}
	a := newTestAgent(llm, tg)

	if !a.runTurn(context.Background(), []Post{post(1, "летит на Аркадию")}) {
		t.Fatal("turn should complete")
	}
	if len(llm.calls) != 1 {
		t.Fatalf("llm calls = %d, want 1", len(llm.calls))
	}
	if len(tg.sent) != 1 || tg.sent[0] != "🚨 Шахеды на Аркадию" || tg.silent[0] {
		t.Fatalf("sent = %q silent = %v", tg.sent, tg.silent)
	}
	roles := []string{}
	for _, m := range a.history[0].msgs {
		roles = append(roles, m.Role)
	}
	if got := strings.Join(roles, ","); got != "user,assistant,tool" {
		t.Fatalf("history roles = %s", got)
	}
}

func TestTurnWithoutToolCallSendsNothing(t *testing.T) {
	llm := &fakeLLM{fn: always(say("повтор"))}
	tg := &fakeMessenger{}
	a := newTestAgent(llm, tg)

	if !a.runTurn(context.Background(), []Post{post(1, "реклама")}) || len(tg.sent) != 0 || len(llm.calls) != 1 {
		t.Fatalf("sent = %q, calls = %d", tg.sent, len(llm.calls))
	}
}

func TestNextRequestExtendsThePreviousOne(t *testing.T) {
	llm := &fakeLLM{fn: always(say("ok"))}
	a := newTestAgent(llm, &fakeMessenger{})
	a.tools.Seed([]Post{{At: time.Now(), Text: "🚨 ранее"}})

	a.runTurn(context.Background(), []Post{post(1, "первое")})
	a.runTurn(context.Background(), []Post{post(2, "второе")})

	first, second := llm.calls[0], llm.calls[1]
	if first[0].Role != "system" || first[0].Content != "SYSTEM" {
		t.Fatalf("first message = %+v", first[0])
	}
	for _, want := range []string{"[состояние]", "воздушная тревога: активна", "🚨 ранее", "src:\nпервое"} {
		if !strings.Contains(first[1].Content, want) {
			t.Fatalf("first turn lacks %q:\n%s", want, first[1].Content)
		}
	}
	if !reflect.DeepEqual(first, second[:len(first)]) {
		t.Fatal("second request must start with the first one byte for byte, or the prompt cache misses")
	}
	last := second[len(second)-1].Content
	if !strings.Contains(last, "src:\nвторое") || strings.Contains(last, "ранее") {
		t.Fatalf("second turn must carry the new post without a recap:\n%s", last)
	}
}

func TestReadToolGetsFollowUpCall(t *testing.T) {
	llm := &fakeLLM{fn: func(n int) (ChatMessage, error) {
		if n == 0 {
			return toolCall(toolGetRecent, `{"channel":"src"}`), nil
		}
		return say("ничего нового"), nil
	}}
	tg := &fakeMessenger{posts: []Post{post(7, "контекст")}}
	a := newTestAgent(llm, tg)

	a.runTurn(context.Background(), []Post{post(8, "что там")})
	if len(llm.calls) != 2 {
		t.Fatalf("llm calls = %d, want 2", len(llm.calls))
	}
	result := llm.calls[1][len(llm.calls[1])-1]
	if result.Role != "tool" || result.ToolCallID != "c1" || !strings.Contains(result.Content, "контекст") {
		t.Fatalf("tool result = %+v", result)
	}
}

func TestToolRoundsAreCapped(t *testing.T) {
	llm := &fakeLLM{fn: always(toolCall(toolGetRecent, `{"channel":"src"}`))}
	a := newTestAgent(llm, &fakeMessenger{})

	a.runTurn(context.Background(), []Post{post(1, "x")})
	if len(llm.calls) != maxToolRounds {
		t.Fatalf("llm calls = %d, want %d", len(llm.calls), maxToolRounds)
	}
}

func TestFailedTurnIsRetriedOnlyBeforeAnAlert(t *testing.T) {
	boom := errors.New("boom")

	llm := &fakeLLM{fn: func(int) (ChatMessage, error) { return ChatMessage{}, boom }}
	a := newTestAgent(llm, &fakeMessenger{})
	if a.runTurn(context.Background(), []Post{post(1, "x")}) || len(a.history) != 0 {
		t.Fatal("failure before any alert must ask for a retry and keep history clean")
	}

	// The unknown tool forces a follow-up call, which fails after the alert already went out.
	llm = &fakeLLM{fn: func(n int) (ChatMessage, error) {
		if n == 0 {
			msg := toolCall(toolSendAlert, dangerArgs)
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: "c2", Function: FunctionCall{Name: "nope"}})
			return msg, nil
		}
		return ChatMessage{}, boom
	}}
	tg := &fakeMessenger{}
	a = newTestAgent(llm, tg)
	if !a.runTurn(context.Background(), []Post{post(1, "x")}) || len(tg.sent) != 1 || len(a.history) != 1 {
		t.Fatalf("failure after an alert must not be retried: sent = %d, history = %d", len(tg.sent), len(a.history))
	}
}

func TestRunBatchesPostsThatArriveDuringACall(t *testing.T) {
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	llm := &fakeLLM{fn: func(n int) (ChatMessage, error) {
		started <- struct{}{}
		if n == 0 {
			<-release
		}
		return say("skip"), nil
	}}
	a := newTestAgent(llm, &fakeMessenger{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	a.Enqueue(post(1, "first"))
	<-started
	a.Enqueue(post(2, "second"))
	a.Enqueue(post(3, "third"))
	close(release)
	<-started
	cancel()
	<-done

	if first := llm.lastUser(0); !strings.Contains(first, "first") || strings.Contains(first, "second") {
		t.Fatalf("first turn must hold only the first post:\n%s", first)
	}
	if next := llm.lastUser(1); !strings.Contains(next, "second") || !strings.Contains(next, "third") {
		t.Fatalf("second turn must batch the queued posts:\n%s", next)
	}
}

func TestRunRetriesAfterLLMFailure(t *testing.T) {
	llm := &fakeLLM{fn: func(n int) (ChatMessage, error) {
		if n == 0 {
			return ChatMessage{}, errors.New("down")
		}
		return toolCall(toolSendAlert, dangerArgs), nil
	}}
	tg := &fakeMessenger{notify: make(chan struct{}, 1)}
	a := newTestAgent(llm, tg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	a.Enqueue(post(1, "критично"))
	select {
	case <-tg.notify:
	case <-time.After(5 * time.Second):
		t.Fatal("alert was not sent after retry")
	}
	cancel()
	<-done

	if !strings.Contains(llm.lastUser(1), "критично") {
		t.Fatal("retry must carry the original post")
	}
}

func TestSystemNoteWaitsForTheNextPost(t *testing.T) {
	started := make(chan struct{}, 2)
	llm := &fakeLLM{fn: func(int) (ChatMessage, error) {
		started <- struct{}{}
		return say("skip"), nil
	}}
	a := newTestAgent(llm, &fakeMessenger{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	a.Enqueue(Post{At: time.Now().Add(-time.Hour), Channel: systemChannel, Text: alertEndNote})
	select {
	case <-started:
		t.Fatal("an alert note alone must not start a turn")
	case <-time.After(50 * time.Millisecond):
	}

	a.Enqueue(post(1, "новый пост"))
	<-started
	cancel()
	<-done

	if got := llm.lastUser(0); !strings.Contains(got, alertEndNote) || !strings.Contains(got, "новый пост") {
		t.Fatalf("the note must reach the agent with the next post, however old it is:\n%s", got)
	}
}

func TestStalePostsAreDroppedAndSorted(t *testing.T) {
	a := newTestAgent(&fakeLLM{}, &fakeMessenger{})
	now := time.Now()
	got := a.fresh([]Post{
		{ID: 3, At: now},
		{ID: 1, At: now.Add(-maxPostAge - time.Second)},
		{ID: 2, At: now.Add(-time.Minute)},
	})
	if len(got) != 2 || got[0].ID != 2 || got[1].ID != 3 {
		t.Fatalf("fresh = %+v", got)
	}
}

func TestHistoryTrimsRarelyAndStaysInBudget(t *testing.T) {
	const turns = 80
	llm := &fakeLLM{fn: always(say("ok"))}
	a := newTestAgent(llm, &fakeMessenger{})
	a.budget = 600
	limit := int(float64(a.budget) * defaultBytesPerToken)

	for i := range turns {
		a.runTurn(context.Background(), []Post{post(i, "шахеды над морем, курс на город")})
	}

	turnSize := a.history[0].size
	trims := 0
	for n := 1; n < turns; n++ {
		prev, cur := llm.calls[n-1], llm.calls[n]
		if size := sizeOf(cur); size > limit+turnSize {
			t.Fatalf("request %d is %d bytes, budget is %d", n, size, limit)
		}
		if len(cur) > len(prev) && reflect.DeepEqual(prev, cur[:len(prev)]) {
			continue
		}
		trims++
		if !strings.Contains(cur[len(cur)-1].Content, "оповещений") {
			t.Fatalf("turn %d follows a trim and must recap sent alerts", n)
		}
	}
	if trims == 0 || trims > turns/5 {
		t.Fatalf("trims = %d in %d turns: the prefix must survive many turns between trims", trims, turns)
	}
}

func TestBudgetCalibratesFromReportedUsage(t *testing.T) {
	const bytesPerToken = 6
	llm := &fakeLLM{fn: always(say("ok"))}
	llm.usage = func(msgs []ChatMessage) Usage { return Usage{Prompt: sizeOf(msgs) / bytesPerToken} }
	a := newTestAgent(llm, &fakeMessenger{})
	a.budget = 300

	for i := range 60 {
		a.runTurn(context.Background(), []Post{post(i, "шахеды над морем, курс на город")})
	}
	if a.bytesPerToken < bytesPerToken-0.5 || a.bytesPerToken > bytesPerToken+0.5 {
		t.Fatalf("bytesPerToken = %.2f, want ~%d", a.bytesPerToken, bytesPerToken)
	}
	peak := 0
	for _, call := range llm.calls {
		peak = max(peak, llm.usage(call).Prompt)
	}
	if slack := a.budget / 5; peak < a.budget/2 || peak > a.budget+slack {
		t.Fatalf("peak prompt = %d tokens, budget = %d", peak, a.budget)
	}

	llm.usage = func([]ChatMessage) Usage { return Usage{Prompt: 1} }
	a.runTurn(context.Background(), []Post{post(1, "x")})
	if a.bytesPerToken != 8 {
		t.Fatalf("bytesPerToken = %.2f, want the upper clamp", a.bytesPerToken)
	}
}

func TestIdleHistoryExpires(t *testing.T) {
	llm := &fakeLLM{fn: always(say("ok"))}
	a := newTestAgent(llm, &fakeMessenger{})
	clock := time.Now()
	a.now = func() time.Time { return clock }

	a.runTurn(context.Background(), []Post{{At: clock, Channel: "src", Text: "старый инцидент"}})
	clock = clock.Add(historyTTL + time.Second)
	a.runTurn(context.Background(), []Post{{At: clock, Channel: "src", Text: "новый инцидент"}})

	last := llm.calls[1]
	if len(last) != 2 || !strings.Contains(last[1].Content, "оповещений") {
		t.Fatalf("stale history must be dropped and replaced by a recap: %+v", last)
	}
}

func TestCleanTextTruncatesByRunes(t *testing.T) {
	if got := cleanText("  привет \n"); got != "привет" {
		t.Fatalf("got %q", got)
	}
	long := cleanText(strings.Repeat("ж", maxPostRunes+10))
	if n := len([]rune(long)); n != maxPostRunes+1 || !strings.HasSuffix(long, "…") {
		t.Fatalf("runes = %d", n)
	}
}

func TestPromptFileReloadsOnChange(t *testing.T) {
	path := t.TempDir() + "/prompt.txt"
	if _, err := newPromptFile(path); err == nil {
		t.Fatal("missing file must fail")
	}
	os.WriteFile(path, []byte("v1\n"), 0o600)
	p, err := newPromptFile(path)
	if err != nil || p.Text() != "v1" {
		t.Fatalf("text = %q, err = %v", p.Text(), err)
	}
	os.WriteFile(path, []byte("v2"), 0o600)
	os.Chtimes(path, time.Now(), time.Now().Add(time.Hour))
	if p.Text() != "v2" {
		t.Fatalf("text = %q, want v2", p.Text())
	}
	os.WriteFile(path, nil, 0o600)
	os.Chtimes(path, time.Now(), time.Now().Add(2*time.Hour))
	if p.Text() != "v2" {
		t.Fatal("empty file must keep the previous prompt")
	}
}

type staticLLM struct{ reply ChatMessage }

func (s staticLLM) Chat(context.Context, []ChatMessage, []ToolDef) (ChatMessage, Usage, error) {
	return s.reply, Usage{}, nil
}

// BenchmarkTurn measures the agent's own cost per turn with a full context.
func BenchmarkTurn(b *testing.B) {
	a := newTestAgent(staticLLM{reply: say("skip")}, &fakeMessenger{})
	text := strings.Repeat("Шахеды курсом на Одессу со стороны моря. ", 8)
	posts := []Post{post(1, text), post(2, text), post(3, text)}
	for range 200 {
		a.runTurn(context.Background(), posts)
	}
	b.ReportAllocs()
	for b.Loop() {
		a.runTurn(context.Background(), posts)
	}
}
