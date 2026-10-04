package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/gotd/td/tgerr"
)

const (
	pollInterval   = 5 * time.Second
	pollLimit      = 10
	seenPerChannel = 128
)

type Source struct {
	Name string
	// Push is set when the account has joined the channel and Telegram pushes its posts.
	Push bool
}

// Intake merges pushed updates with the poll and passes each post on once.
type Intake struct {
	reconcile func() bool
	emit      func(Post)
	now       func() time.Time

	mu   sync.Mutex
	seen map[string]*idRing
}

// reconcile tells when pushed channels are polled as well.
func NewIntake(reconcile func() bool, emit func(Post)) *Intake {
	return &Intake{reconcile: reconcile, emit: emit, now: time.Now, seen: make(map[string]*idRing)}
}

func (in *Intake) Accept(p Post) {
	if p.Text = cleanText(p.Text); p.Text == "" || in.now().Sub(p.At) > maxPostAge {
		return
	}
	in.mu.Lock()
	ring := in.seen[p.Channel]
	if ring == nil {
		ring = &idRing{ids: make(map[int]struct{}, seenPerChannel)}
		in.seen[p.Channel] = ring
	}
	first := ring.add(p.ID)
	in.mu.Unlock()
	if first {
		in.emit(p)
	}
}

func (in *Intake) Run(ctx context.Context, tg Messenger, sources []Source) error {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		in.poll(ctx, tg, sources)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (in *Intake) poll(ctx context.Context, tg Messenger, sources []Source) {
	reconcile := in.reconcile()
	for _, src := range sources {
		if src.Push && !reconcile {
			continue
		}
		posts, err := tg.Recent(ctx, src.Name, pollLimit)
		if err != nil {
			slog.Warn("poll failed", "channel", src.Name, "err", err)
			if wait, ok := tgerr.AsFloodWait(err); ok {
				select {
				case <-ctx.Done():
				case <-time.After(wait):
				}
				return
			}
			continue
		}
		for _, p := range posts {
			in.Accept(p)
		}
	}
}

// idRing remembers the last seenPerChannel message IDs; Telegram IDs start at 1.
type idRing struct {
	ids  map[int]struct{}
	ring [seenPerChannel]int
	next int
}

func (r *idRing) add(id int) bool {
	if _, ok := r.ids[id]; ok {
		return false
	}
	if old := r.ring[r.next]; old != 0 {
		delete(r.ids, old)
	}
	r.ring[r.next] = id
	r.ids[id] = struct{}{}
	r.next = (r.next + 1) % seenPerChannel
	return true
}
