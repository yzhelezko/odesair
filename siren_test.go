package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

type fakeSiren struct {
	active bool
	err    error
}

func (f *fakeSiren) check(context.Context) (bool, error) { return f.active, f.err }

// changes records the alert transitions a Siren reports.
type changes struct{ active []bool }

func (c *changes) record(_ context.Context, active bool, _ time.Time) {
	c.active = append(c.active, active)
}

func TestSirenTransitionsAndGrace(t *testing.T) {
	api := &fakeSiren{}
	var got changes
	clock := time.Now()
	s := NewSiren(api.check, 10*time.Minute)
	s.now = func() time.Time { return clock }
	poll := func() { s.Poll(context.Background(), got.record) }

	if !s.Open() || s.Status() != AlertUnknown {
		t.Fatal("unknown state must fail open")
	}

	poll()
	if s.Open() || s.Status() != AlertInactive || len(got.active) != 0 {
		t.Fatalf("first observation: open = %v, status = %v, changes = %v", s.Open(), s.Status(), got.active)
	}

	api.active = true
	poll()
	poll()
	if !s.Open() || !slices.Equal(got.active, []bool{true}) {
		t.Fatalf("alert start must be reported once: open = %v, changes = %v", s.Open(), got.active)
	}

	api.active = false
	poll()
	if !s.Open() || !slices.Equal(got.active, []bool{true, false}) {
		t.Fatalf("alert end must keep the window open for the grace period: changes = %v", got.active)
	}
	clock = clock.Add(10 * time.Minute)
	poll()
	if s.Open() || len(got.active) != 2 {
		t.Fatal("window must close after the grace period")
	}
}

func TestSirenStartsDuringAlertWithoutChange(t *testing.T) {
	api := &fakeSiren{active: true}
	var got changes
	s := NewSiren(api.check, time.Minute)
	s.Poll(context.Background(), got.record)
	if !s.Open() || s.Status() != AlertActive || len(got.active) != 0 {
		t.Fatalf("open = %v, status = %v, changes = %v", s.Open(), s.Status(), got.active)
	}
}

func TestSirenFailsOpenAfterRepeatedErrors(t *testing.T) {
	api := &fakeSiren{}
	var got changes
	s := NewSiren(api.check, 0)
	poll := func() { s.Poll(context.Background(), got.record) }

	poll()
	api.err = errors.New("timeout")
	for range sirenMaxFails - 1 {
		poll()
	}
	if s.Open() {
		t.Fatal("a few failures must keep the last known state")
	}
	poll()
	if !s.Open() {
		t.Fatal("persistent failures must open the window")
	}
	api.err = nil
	poll()
	if s.Open() || len(got.active) != 0 {
		t.Fatal("recovery must restore the window without reporting a change")
	}
}

func TestSirenCheckParsesAPI(t *testing.T) {
	body := `[{"regionId":"964","activeAlerts":[{"type":"ARTILLERY"},{"type":"AIR"}]}]`
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	defer srv.Close()
	check := sirenCheck(srv.URL)

	if active, err := check(context.Background()); err != nil || !active {
		t.Fatalf("active = %v, err = %v", active, err)
	}
	body = `[{"regionId":"964","activeAlerts":[]}]`
	if active, err := check(context.Background()); err != nil || active {
		t.Fatalf("active = %v, err = %v", active, err)
	}
	status = http.StatusBadGateway
	if _, err := check(context.Background()); err == nil {
		t.Fatal("non-200 must be an error")
	}
}
