package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/updates"
	updhook "github.com/gotd/td/telegram/updates/hook"
	"github.com/gotd/td/tg"
	"golang.org/x/sync/errgroup"
)

func main() {
	var level slog.Level
	if err := level.UnmarshalText([]byte(getEnv("LOG_LEVEL", "info"))); err != nil {
		level = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	var err error
	if len(os.Args) == 2 && os.Args[1] == "login" {
		err = login(ctx, getEnv("OPENAI_AUTH_FILE", defaultAuthFile), os.Stdout)
	} else {
		err = run(ctx)
	}
	stop()
	if err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return err
	}
	prompt, err := newPromptFile(promptPath)
	if err != nil {
		return fmt.Errorf("system prompt: %w", err)
	}
	endpoint := cfg.LLM.BaseURL
	if cfg.LLM.OpenAI {
		endpoint = openaiResponsesURL
	}
	slog.Info("config", "llm", endpoint, "model", cfg.LLM.Model, "effort", cfg.LLM.Effort,
		"context_tokens", cfg.LLM.ContextTokens, "sources", cfg.Sources, "out", cfg.OutChannel,
		"send", cfg.SendEnabled)

	tgc := newTelegram()
	siren := NewSiren(sirenCheck(sirenURL), cfg.AlertGrace)
	tools := NewToolbox(tgc, cfg.Sources, !cfg.SendEnabled, loc)
	llm := newLLM(cfg.LLM)
	if n, ok := llm.(interface{ Notice() string }); ok {
		tools.notice = n.Notice
	}
	agent := NewAgent(llm, tools, prompt.Text, siren.Status, loc, cfg.LLM.ContextTokens)
	intake := NewIntake(siren.Open, agent.Enqueue)

	// The dispatcher map is not synchronised: register before the client starts.
	dispatcher := tg.NewUpdateDispatcher()
	dispatcher.OnNewChannelMessage(tgc.Handler(intake.Accept))
	gaps := updates.New(updates.Config{Handler: dispatcher, AccessHasher: tgc})

	client := telegram.NewClient(cfg.AppID, cfg.AppHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: sessionPath},
		UpdateHandler:  gaps,
		Middlewares:    []telegram.Middleware{updhook.UpdateHook(gaps.Handle)},
	})
	tgc.api = client.API()

	return client.Run(ctx, func(ctx context.Context) error {
		if err := authenticate(ctx, client, cfg); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
		self, err := client.Self(ctx)
		if err != nil {
			return fmt.Errorf("self: %w", err)
		}
		sources, err := tgc.Resolve(ctx, cfg.OutChannel, cfg.Sources)
		if err != nil {
			return err
		}
		if posts, err := tgc.Recent(ctx, cfg.OutChannel, recapAlerts); err != nil {
			slog.Warn("alert memory not restored", "err", err)
		} else {
			tools.Seed(posts)
		}

		siren.Poll(ctx, agent.Enqueue)
		g, ctx := errgroup.WithContext(ctx)
		g.Go(func() error { return siren.Run(ctx, agent.Enqueue) })
		g.Go(func() error {
			return gaps.Run(ctx, client.API(), self.ID, updates.AuthOptions{
				OnStart: func(context.Context) { slog.Info("updates started") },
			})
		})
		g.Go(func() error { return intake.Run(ctx, tgc, sources) })
		g.Go(func() error { return agent.Run(ctx) })
		return g.Wait()
	})
}
