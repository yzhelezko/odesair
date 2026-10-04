package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	inboxSize       = 256
	maxPostsPerTurn = 40
	maxToolRounds   = 4
	maxPostAge      = 10 * time.Minute
	maxPostRunes    = 1500
	retryDelay      = 3 * time.Second
	historyTTL      = 30 * time.Minute
	recapAlerts     = 5
	clockLayout     = "15:04:05"

	trimTarget           = 0.6
	defaultBytesPerToken = 3.0
)

type Usage struct {
	Prompt     int
	Cached     int
	Completion int
}

type LLM interface {
	Chat(ctx context.Context, msgs []ChatMessage, tools []ToolDef) (ChatMessage, Usage, error)
}

type turn struct {
	at   time.Time
	size int
	msgs []ChatMessage
}

type Agent struct {
	llm        LLM
	tools      *Toolbox
	prompt     func() string
	alert      func() AlertStatus
	now        func() time.Time
	loc        *time.Location
	retryDelay time.Duration
	inbox      chan Post

	history       []turn
	budget        int
	bytesPerToken float64
	recap         bool
}

func NewAgent(llm LLM, tools *Toolbox, prompt func() string, alert func() AlertStatus, loc *time.Location, budget int) *Agent {
	return &Agent{
		llm:           llm,
		tools:         tools,
		prompt:        prompt,
		alert:         alert,
		now:           time.Now,
		loc:           loc,
		retryDelay:    retryDelay,
		inbox:         make(chan Post, inboxSize),
		budget:        budget,
		bytesPerToken: defaultBytesPerToken,
	}
}

// Enqueue never blocks: it is called from the Telegram update handler.
func (a *Agent) Enqueue(p Post) {
	select {
	case a.inbox <- p:
	default:
		slog.Warn("agent inbox full, post dropped", "channel", p.Channel)
	}
}

func (a *Agent) Run(ctx context.Context) error {
	var pending []Post
	for {
		if len(pending) == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case p := <-a.inbox:
				pending = append(pending, p)
			}
		}
		pending = a.fresh(a.drain(pending))
		if len(pending) == 0 {
			continue
		}
		if a.runTurn(ctx, pending) {
			pending = pending[:0]
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(a.retryDelay):
		}
	}
}

func (a *Agent) drain(pending []Post) []Post {
	for len(pending) < maxPostsPerTurn {
		select {
		case p := <-a.inbox:
			pending = append(pending, p)
		default:
			return pending
		}
	}
	return pending
}

func (a *Agent) fresh(posts []Post) []Post {
	cutoff := a.now().Add(-maxPostAge)
	posts = slices.DeleteFunc(posts, func(p Post) bool { return p.At.Before(cutoff) })
	slices.SortStableFunc(posts, func(x, y Post) int { return x.At.Compare(y.At) })
	return posts
}

// runTurn reports false when the posts have to be retried.
func (a *Agent) runTurn(ctx context.Context, posts []Post) bool {
	wall := time.Now()
	now := a.now()
	if n := len(a.history); n > 0 && now.Sub(a.history[n-1].at) > historyTTL {
		a.history = a.history[:0]
	}

	system := a.prompt()
	msgs := make([]ChatMessage, 0, 4+len(a.history)*3)
	msgs = append(msgs, ChatMessage{Role: "system", Content: system})
	for _, t := range a.history {
		msgs = append(msgs, t.msgs...)
	}
	base := len(msgs)
	msgs = append(msgs, ChatMessage{Role: "user", Content: a.userMessage(now, posts)})

	sends := a.tools.Sends()
	var usage Usage
	reply := ""
	for round := range maxToolRounds {
		msg, u, err := a.llm.Chat(ctx, msgs, a.tools.Defs())
		if err != nil {
			slog.Error("llm call failed", "round", round, "err", err)
			if a.tools.Sends() == sends {
				return false
			}
			break
		}
		if usage = u; u.Prompt > 0 {
			a.bytesPerToken = min(max(float64(sizeOf(msgs))/float64(u.Prompt), 1), 8)
		}
		msgs = append(msgs, msg)
		reply = msg.Content

		again := false
		for _, tc := range msg.ToolCalls {
			res, more := a.tools.Call(ctx, tc)
			again = again || more
			msgs = append(msgs, ChatMessage{Role: "tool", ToolCallID: tc.ID, Content: res})
		}
		if !again {
			break
		}
	}

	a.remember(turn{at: now, msgs: slices.Clone(msgs[base:])}, len(system))
	slog.Info("turn", "posts", len(posts), "alerts", a.tools.Sends()-sends,
		"took", time.Since(wall).Round(time.Millisecond), "prompt_tokens", usage.Prompt,
		"cached_tokens", usage.Cached, "completion_tokens", usage.Completion,
		"history_turns", len(a.history), "reply", reply)
	return true
}

func (a *Agent) userMessage(now time.Time, posts []Post) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[состояние] время %s, воздушная тревога: %s\n", now.In(a.loc).Format(clockLayout), a.alert())
	if len(a.history) == 0 || a.recap {
		a.writeRecap(&b)
	}
	b.WriteByte('\n')
	for _, p := range posts {
		writePost(&b, p, a.loc)
	}
	return b.String()
}

// writeRecap restates sent alerts when history no longer shows them.
func (a *Agent) writeRecap(b *strings.Builder) {
	alerts := a.tools.Alerts()
	if len(alerts) == 0 {
		b.WriteString("Отправленных оповещений за последние часы нет.\n")
		return
	}
	b.WriteString("Последние отправленные оповещения:\n")
	for _, s := range alerts[max(0, len(alerts)-recapAlerts):] {
		prefix := clearPrefix
		if s.danger {
			prefix = dangerPrefix
		}
		fmt.Fprintf(b, "- %s %s %s\n", s.at.In(a.loc).Format(clockLayout), prefix, s.text)
	}
}

func (a *Agent) remember(t turn, systemBytes int) {
	t.size = sizeOf(t.msgs)
	a.history = append(a.history, t)
	a.recap = false

	total := systemBytes
	for _, h := range a.history {
		total += h.size
	}
	budget := float64(a.budget) * a.bytesPerToken
	if float64(total) <= budget {
		return
	}
	drop := 0
	for target := int(budget * trimTarget); drop < len(a.history)-1 && total > target; drop++ {
		total -= a.history[drop].size
	}
	a.history = slices.Delete(a.history, 0, drop)
	a.recap = true
}

func sizeOf(msgs []ChatMessage) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content) + len(m.ReasoningContent)
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Arguments)
		}
	}
	return n
}

func writePost(b *strings.Builder, p Post, loc *time.Location) {
	fmt.Fprintf(b, "[%s] %s:\n%s\n\n", p.At.In(loc).Format(clockLayout), p.Channel, p.Text)
}

func cleanText(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxPostRunes || utf8.RuneCountInString(s) <= maxPostRunes {
		return s
	}
	return string([]rune(s)[:maxPostRunes]) + "…"
}

// promptFile re-reads the system prompt when the file changes on disk.
type promptFile struct {
	path string
	mod  time.Time
	text string
}

func newPromptFile(path string) (*promptFile, error) {
	p := &promptFile{path: path}
	if err := p.load(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *promptFile) load() error {
	st, err := os.Stat(p.path)
	if err != nil {
		return err
	}
	if st.ModTime().Equal(p.mod) {
		return nil
	}
	raw, err := os.ReadFile(p.path)
	if err != nil {
		return err
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return fmt.Errorf("%s is empty", p.path)
	}
	p.text, p.mod = text, st.ModTime()
	slog.Info("system prompt loaded", "path", p.path, "bytes", len(text))
	return nil
}

func (p *promptFile) Text() string {
	if err := p.load(); err != nil {
		slog.Warn("system prompt reload failed, keeping previous", "err", err)
	}
	return p.text
}
