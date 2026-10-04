package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	maxAlertRunes  = 400
	dedupWindow    = 10 * time.Minute
	rateWindow     = 5 * time.Minute
	rateLimit      = 15
	alertMemory    = 3 * time.Hour
	recentWindow   = 2 * time.Hour
	recentDefault  = 10
	recentMax      = 20
	dangerPrefix   = "🚨"
	clearPrefix    = "✅"
	toolSendAlert  = "send_alert"
	toolGetRecent  = "get_recent_messages"
	alertPrefixCut = dangerPrefix + clearPrefix + " \n"
	noticeSep      = "\n\n"

	// Fixed posts for the air alert itself, and the notes that tell the agent about them.
	systemChannel  = "система"
	alertStartPost = "📢 Воздушная тревога в Одессе."
	alertEndText   = "Отбой воздушной тревоги в Одессе."
	alertStartNote = "Объявлена воздушная тревога в Одессе. Автоматическое сообщение об этом опубликовано в канале."
	alertEndNote   = "Отбой воздушной тревоги в Одессе. Автоматическое сообщение об отбое опубликовано в канале."
)

type Post struct {
	ID      int
	At      time.Time
	Channel string
	Text    string
}

type Messenger interface {
	Send(ctx context.Context, text string, silent bool) error
	// Recent returns the latest posts of a channel, oldest first.
	Recent(ctx context.Context, channel string, limit int) ([]Post, error)
}

type sentAlert struct {
	at     time.Time
	danger bool
	text   string
}

// Toolbox belongs to the agent goroutine, except Announce, which the siren
// goroutine calls; mu guards the alert memory they share.
type Toolbox struct {
	tg      Messenger
	sources []string
	dryRun  bool
	now     func() time.Time
	loc     *time.Location
	defs    []ToolDef
	sends   int
	// notice, when it returns text, is appended to every post.
	notice func() string

	mu   sync.Mutex
	sent []sentAlert
}

func NewToolbox(tg Messenger, sources []string, dryRun bool, loc *time.Location) *Toolbox {
	return &Toolbox{
		tg:      tg,
		sources: sources,
		dryRun:  dryRun,
		now:     time.Now,
		loc:     loc,
		defs:    toolDefs(sources),
	}
}

func toolDefs(sources []string) []ToolDef {
	object := func(props map[string]any, required ...string) json.RawMessage {
		b, err := json.Marshal(map[string]any{"type": "object", "properties": props, "required": required})
		if err != nil {
			panic(err)
		}
		return b
	}
	return []ToolDef{
		{Type: "function", Function: FunctionDef{
			Name:        toolSendAlert,
			Description: "Опубликовать оповещение в Telegram-канале. Это единственный способ что-либо сообщить подписчикам. Вызывай только когда этого требуют правила.",
			Parameters: object(map[string]any{
				"reason": map[string]any{"type": "string", "description": "Кратко, почему оповещение нужно, со ссылкой на номер правила. Не публикуется."},
				"danger": map[string]any{"type": "boolean", "description": "true — есть угроза для Одессы (🚨, со звуком). false — угроза миновала или отбой (✅, без звука)."},
				"text":   map[string]any{"type": "string", "description": fmt.Sprintf("Краткий информативный текст оповещения на русском, без эмодзи в начале, до %d символов.", maxAlertRunes)},
			}, "reason", "danger", "text"),
		}},
		{Type: "function", Function: FunctionDef{
			Name:        toolGetRecent,
			Description: "Получить последние сообщения канала-источника за последние 2 часа. Используй, только если не хватает контекста, например после перезапуска.",
			Parameters: object(map[string]any{
				"channel": map[string]any{"type": "string", "enum": sources},
				"limit":   map[string]any{"type": "integer", "minimum": 1, "maximum": recentMax},
			}, "channel"),
		}},
	}
}

func (t *Toolbox) Defs() []ToolDef { return t.defs }

// Sends is the number of alerts the agent sent since start.
func (t *Toolbox) Sends() int { return t.sends }

// Alerts returns alerts sent within alertMemory, oldest first.
func (t *Toolbox) Alerts() []sentAlert {
	cutoff := t.now().Add(-alertMemory)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sent = slices.DeleteFunc(t.sent, func(s sentAlert) bool { return s.at.Before(cutoff) })
	return slices.Clone(t.sent)
}

func (t *Toolbox) remember(s sentAlert) {
	t.mu.Lock()
	t.sent = append(t.sent, s)
	t.mu.Unlock()
}

// Seed restores alert memory from the output channel's own history.
func (t *Toolbox) Seed(posts []Post) {
	for _, p := range posts {
		danger := strings.HasPrefix(p.Text, dangerPrefix)
		if !danger && !strings.HasPrefix(p.Text, clearPrefix) {
			continue
		}
		text, _, _ := strings.Cut(p.Text, noticeSep+warnPrefix)
		t.remember(sentAlert{at: p.At, danger: danger, text: strings.TrimLeft(text, alertPrefixCut)})
	}
}

func (t *Toolbox) withNotice(post string) string {
	if t.notice != nil {
		if notice := t.notice(); notice != "" {
			return post + noticeSep + notice
		}
	}
	return post
}

// Announce posts the fixed message for the alert starting or ending and
// returns the note that tells the agent about it. The all-clear counts as a
// sent alert, so the agent's status goes back to safe.
func (t *Toolbox) Announce(ctx context.Context, active bool, at time.Time) Post {
	post, note := alertStartPost, alertStartNote
	if !active {
		post, note = clearPrefix+" "+alertEndText, alertEndNote
		t.remember(sentAlert{at: at, text: alertEndText})
	}
	if !t.dryRun {
		if err := t.tg.Send(ctx, t.withNotice(post), true); err != nil {
			slog.Error("alert announcement not posted", "active", active, "err", err)
		}
	}
	slog.Info("alert announced", "active", active, "dry_run", t.dryRun)
	return Post{At: at, Channel: systemChannel, Text: note}
}

// Call runs one tool call. again reports whether the model has to see the result.
func (t *Toolbox) Call(ctx context.Context, tc ToolCall) (result string, again bool) {
	var err error
	switch tc.Function.Name {
	case toolSendAlert:
		if err = t.sendAlert(ctx, tc.Function.Arguments); err == nil {
			return "Отправлено.", false
		}
	case toolGetRecent:
		if result, err = t.recent(ctx, tc.Function.Arguments); err == nil {
			return result, true
		}
	default:
		err = fmt.Errorf("неизвестный инструмент %q", tc.Function.Name)
	}
	slog.Warn("tool call rejected", "tool", tc.Function.Name, "err", err)
	return "Ошибка: " + err.Error(), true
}

func (t *Toolbox) sendAlert(ctx context.Context, args string) error {
	var a struct {
		Reason string `json:"reason"`
		Danger *bool  `json:"danger"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return fmt.Errorf("некорректные аргументы: %w", err)
	}
	text := strings.TrimSpace(strings.TrimLeft(a.Text, alertPrefixCut))
	switch {
	case a.Danger == nil:
		return errors.New("не указан danger")
	case text == "":
		return errors.New("пустой text")
	case utf8.RuneCountInString(text) > maxAlertRunes:
		return fmt.Errorf("text длиннее %d символов, сократи", maxAlertRunes)
	}
	danger := *a.Danger

	now := t.now()
	alerts := t.Alerts()
	if n := len(alerts); n > 0 {
		last := alerts[n-1]
		if last.danger == danger && last.text == text && now.Sub(last.at) < dedupWindow {
			return errors.New("дубликат предыдущего оповещения, не отправлено")
		}
	}
	recent := 0
	for _, s := range alerts {
		if now.Sub(s.at) < rateWindow {
			recent++
		}
	}
	if recent >= rateLimit {
		return errors.New("превышен лимит оповещений, не отправлено")
	}

	prefix := clearPrefix
	if danger {
		prefix = dangerPrefix
	}
	if !t.dryRun {
		if err := t.tg.Send(ctx, t.withNotice(prefix+" "+text), !danger); err != nil {
			return fmt.Errorf("telegram: %w", err)
		}
	}
	t.remember(sentAlert{at: now, danger: danger, text: text})
	t.sends++
	slog.Info("alert sent", "danger", danger, "dry_run", t.dryRun, "text", text, "reason", a.Reason)
	return nil
}

func (t *Toolbox) recent(ctx context.Context, args string) (string, error) {
	var a struct {
		Channel string `json:"channel"`
		Limit   int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return "", fmt.Errorf("некорректные аргументы: %w", err)
	}
	if !slices.Contains(t.sources, a.Channel) {
		return "", fmt.Errorf("неизвестный канал, доступны: %s", strings.Join(t.sources, ", "))
	}
	if a.Limit == 0 {
		a.Limit = recentDefault
	}
	posts, err := t.tg.Recent(ctx, a.Channel, min(max(a.Limit, 1), recentMax))
	if err != nil {
		return "", fmt.Errorf("telegram: %w", err)
	}

	cutoff := t.now().Add(-recentWindow)
	var b strings.Builder
	for _, p := range posts {
		if p.Text = cleanText(p.Text); p.Text != "" && !p.At.Before(cutoff) {
			writePost(&b, p, t.loc)
		}
	}
	if b.Len() == 0 {
		return "Нет сообщений за последние 2 часа.", nil
	}
	return b.String(), nil
}
