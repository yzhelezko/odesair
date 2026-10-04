package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	sessionPath = "config/tdlib-session"
	promptPath  = "config/system_message.txt"
	sirenURL    = "https://siren.pp.ua/api/v3/alerts/964"
	timezone    = "Europe/Kyiv"

	defaultLLMBaseURL = "https://api.z.ai/api/coding/paas/v4"
	defaultLLMModel   = "glm-5.3"
	defaultLLMEffort  = "low"
	minContextTokens  = 4000
)

type Config struct {
	AppID    int
	AppHash  string
	Phone    string
	Password string

	Sources     []string
	OutChannel  string
	SendEnabled bool

	LLMBaseURL    string
	LLMModel      string
	LLMKey        string
	LLMEffort     string
	ContextTokens int

	AlertGrace time.Duration
}

func loadConfig() (Config, error) {
	var errs []error
	required := func(key string) string {
		v := getEnv(key, "")
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", key))
		}
		return v
	}
	boolean := func(key string, fallback bool) bool {
		v, err := strconv.ParseBool(getEnv(key, strconv.FormatBool(fallback)))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
		}
		return v
	}

	cfg := Config{
		AppHash:     required("APPHASH"),
		Phone:       required("PHONE_NUMBER"),
		Password:    getEnv("TG_PASSWORD", ""),
		LLMKey:      required("API_KEY"),
		Sources:     splitList(getEnv("SOURCE_CHANNELS", "xydessa_live,freechat_odesa,odesairxydessa,Sila_GO")),
		OutChannel:  getEnv("SEND_TO_CHANNEL", "odesair"),
		SendEnabled: boolean("ENABLE_TELEGRAM_SEND", true),
		LLMBaseURL:  strings.TrimRight(getEnv("LLM_BASE_URL", defaultLLMBaseURL), "/"),
		LLMModel:    getEnv("LLM_MODEL", defaultLLMModel),
		LLMEffort:   getEnv("LLM_EFFORT", defaultLLMEffort),
	}

	var err error
	if cfg.ContextTokens, err = strconv.Atoi(getEnv("LLM_CONTEXT_TOKENS", "32000")); err != nil || cfg.ContextTokens < minContextTokens {
		errs = append(errs, fmt.Errorf("LLM_CONTEXT_TOKENS must be an integer >= %d", minContextTokens))
	}
	if cfg.AppID, err = strconv.Atoi(required("APPID")); err != nil {
		errs = append(errs, fmt.Errorf("APPID: %w", err))
	}
	if cfg.AlertGrace, err = time.ParseDuration(getEnv("ALERT_GRACE", "10m")); err != nil {
		errs = append(errs, fmt.Errorf("ALERT_GRACE: %w", err))
	}
	if len(cfg.Sources) == 0 {
		errs = append(errs, errors.New("SOURCE_CHANNELS is empty"))
	}
	return cfg, errors.Join(errs...)
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return strings.TrimSpace(v)
	}
	return fallback
}

func splitList(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimPrefix(strings.TrimSpace(p), "@"); p != "" {
			out = append(out, p)
		}
	}
	return out
}
