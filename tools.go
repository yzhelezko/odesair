package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
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

// Toolbox is used from the agent goroutine only.
type Toolbox struct {
	tg      Messenger
	sources []string
	dryRun  bool
	now     func() time.Time
	loc     *time.Location
	defs    []ToolDef
	sent    []sentAlert
	sends   int
	// notice, when it returns text, is appended to the posted alert.
	notice func() string
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

// Sends is the number of alerts sent since start.
func (t *Toolbox) Sends() int { return t.sends }

// Alerts returns alerts sent within alertMemory, oldest first.
func (t *Toolbox) Alerts() []sentAlert {
	cutoff := t.now().Add(-alertMemory)
	t.sent = slices.DeleteFunc(t.sent, func(s sentAlert) bool { return s.at.Before(cutoff) })
	return t.sent
}

// Seed restores alert memory from the output channel's own history.
func (t *Toolbox) Seed(posts []Post) {
	for _, p := range posts {
		danger := strings.HasPrefix(p.Text, dangerPrefix)
		if !danger && !strings.HasPrefix(p.Text, clearPrefix) {
			continue
		}
		text, _, _ := strings.Cut(p.Text, noticeSep+warnPrefix)
		t.sent = append(t.sent, sentAlert{at: p.At, danger: danger, text: strings.TrimLeft(text, alertPrefixCut)})
	}
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
	post := prefix + " " + text
	if t.notice != nil {
		if notice := t.notice(); notice != "" {
			post += noticeSep + notice
		}
	}
	if !t.dryRun {
		if err := t.tg.Send(ctx, post, !danger); err != nil {
			return fmt.Errorf("telegram: %w", err)
		}
	}
	t.sent = append(t.sent, sentAlert{at: now, danger: danger, text: text})
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
