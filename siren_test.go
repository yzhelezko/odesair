package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeSiren struct {
	active bool
	err    error
}

func (f *fakeSiren) check(context.Context) (bool, error) { return f.active, f.err }

func TestSirenTransitionsAndGrace(t *testing.T) {
	api := &fakeSiren{}
	var out sink
	clock := time.Now()
	s := NewSiren(api.check, 10*time.Minute)
	s.now = func() time.Time { return clock }
	poll := func() { s.Poll(context.Background(), out.emit) }

	if !s.Open() || s.Status() != AlertUnknown {
		t.Fatal("unknown state must fail open")
	}

	poll()
	if s.Open() || s.Status() != AlertInactive || len(out.posts) != 0 {
		t.Fatalf("first observation: open = %v, status = %v, events = %d", s.Open(), s.Status(), len(out.posts))
	}

	api.active = true
	poll()
	poll()
	if !s.Open() || len(out.posts) != 1 || out.posts[0].Text != alertStarted || out.posts[0].Channel != systemChannel {
		t.Fatalf("alert start: open = %v, events = %+v", s.Open(), out.posts)
	}

	api.active = false
	poll()
	if !s.Open() || len(out.posts) != 2 || out.posts[1].Text != alertEnded {
		t.Fatalf("alert end must keep the gate open for the grace period: events = %+v", out.posts)
	}
	clock = clock.Add(10 * time.Minute)
	poll()
	if s.Open() || len(out.posts) != 2 {
		t.Fatal("gate must close after the grace period")
	}
}

func TestSirenStartsDuringAlertWithoutEvent(t *testing.T) {
	api := &fakeSiren{active: true}
	var out sink
	s := NewSiren(api.check, time.Minute)
	s.Poll(context.Background(), out.emit)
	if !s.Open() || s.Status() != AlertActive || len(out.posts) != 0 {
		t.Fatalf("open = %v, status = %v, events = %d", s.Open(), s.Status(), len(out.posts))
	}
}

func TestSirenFailsOpenAfterRepeatedErrors(t *testing.T) {
	api := &fakeSiren{}
	var out sink
	s := NewSiren(api.check, 0)
	poll := func() { s.Poll(context.Background(), out.emit) }

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
		t.Fatal("persistent failures must open the gate")
	}
	api.err = nil
	poll()
	if s.Open() || len(out.posts) != 0 {
		t.Fatal("recovery must restore the gate without events")
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
