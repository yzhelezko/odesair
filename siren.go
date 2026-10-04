package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const (
	sirenInterval = 10 * time.Second
	sirenTimeout  = 5 * time.Second
	sirenMaxFails = 6
)

// AlertChange is called when the air alert starts or ends.
type AlertChange func(ctx context.Context, active bool, at time.Time)

type AlertStatus int

const (
	AlertUnknown AlertStatus = iota
	AlertActive
	AlertInactive
)

func (s AlertStatus) String() string {
	switch s {
	case AlertActive:
		return "активна"
	case AlertInactive:
		return "нет"
	}
	return "неизвестно"
}

type Siren struct {
	check func(ctx context.Context) (bool, error)
	grace time.Duration
	now   func() time.Time

	mu      sync.Mutex
	status  AlertStatus
	endedAt time.Time
	fails   int
}

func NewSiren(check func(ctx context.Context) (bool, error), grace time.Duration) *Siren {
	return &Siren{check: check, grace: grace, now: time.Now}
}

func (s *Siren) Status() AlertStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Open reports the alert window: the alert itself plus the grace period after it.
// It fails open when the alert state is unknown or the API is down.
func (s *Siren) Open() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != AlertInactive || s.fails >= sirenMaxFails {
		return true
	}
	return s.now().Sub(s.endedAt) < s.grace
}

func (s *Siren) Poll(ctx context.Context, changed AlertChange) {
	active, err := s.check(ctx)
	now := s.now()

	s.mu.Lock()
	if err != nil {
		s.fails++
		fails := s.fails
		s.mu.Unlock()
		slog.Warn("siren check failed", "fails", fails, "err", err)
		return
	}
	prev := s.status
	s.fails = 0
	s.status = AlertInactive
	if active {
		s.status = AlertActive
	} else if prev == AlertActive {
		s.endedAt = now
	}
	next := s.status
	s.mu.Unlock()

	if prev == next {
		return
	}
	slog.Info("air alert", "status", next.String())
	if prev != AlertUnknown {
		changed(ctx, active, now)
	}
}

func (s *Siren) Run(ctx context.Context, changed AlertChange) error {
	t := time.NewTicker(sirenInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			s.Poll(ctx, changed)
		}
	}
}

func sirenCheck(url string) func(ctx context.Context) (bool, error) {
	client := &http.Client{Timeout: sirenTimeout}
	return func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return false, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false, fmt.Errorf("siren status %d", resp.StatusCode)
		}
		var regions []struct {
			ActiveAlerts []struct {
				Type string `json:"type"`
			} `json:"activeAlerts"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&regions); err != nil {
			return false, err
		}
		for _, r := range regions {
			for _, a := range r.ActiveAlerts {
				if a.Type == "AIR" {
					return true, nil
				}
			}
		}
		return false, nil
	}
}
