package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

type sink struct{ posts []Post }

func (s *sink) emit(p Post) { s.posts = append(s.posts, p) }

func yes() bool { return true }

func TestIntakeDeliversEachPostOnce(t *testing.T) {
	var out sink
	in := NewIntake(yes, out.emit)
	now := time.Now()

	in.Accept(Post{ID: 5, At: now, Channel: "a", Text: " push "})
	in.Accept(Post{ID: 5, At: now, Channel: "a", Text: "poll copy"})
	in.Accept(Post{ID: 5, At: now, Channel: "b", Text: "other channel"})
	in.Accept(Post{ID: 4, At: now, Channel: "a", Text: "missed by push"})
	in.Accept(Post{ID: 6, At: now, Channel: "a", Text: "   "})
	in.Accept(Post{ID: 7, At: now.Add(-maxPostAge - time.Second), Channel: "a", Text: "old"})

	if len(out.posts) != 3 || out.posts[0].Text != "push" || out.posts[1].Channel != "b" || out.posts[2].ID != 4 {
		t.Fatalf("posts = %+v", out.posts)
	}
}

func TestIntakePollsPushedChannelsOnlyWhenReconciling(t *testing.T) {
	var out sink
	reconcile := false
	in := NewIntake(func() bool { return reconcile }, out.emit)
	tg := &fakeMessenger{posts: []Post{{ID: 1, At: time.Now(), Text: "x"}}}
	sources := []Source{{Name: "joined", Push: true}, {Name: "public"}}

	in.poll(context.Background(), tg, sources)
	if len(out.posts) != 1 || out.posts[0].Channel != "public" {
		t.Fatalf("outside an alert only channels without push are polled: %+v", out.posts)
	}

	reconcile = true
	in.poll(context.Background(), tg, sources)
	in.poll(context.Background(), tg, sources)
	if len(out.posts) != 2 || out.posts[1].Channel != "joined" {
		t.Fatalf("during an alert every channel is polled, each post once: %+v", out.posts)
	}

	tg.err = errors.New("rpc")
	in.poll(context.Background(), tg, sources)
	if len(out.posts) != 2 {
		t.Fatal("failed poll must not emit")
	}
}

func TestIntakeRunPollsAtOnceAndStopsOnCancel(t *testing.T) {
	var out sink
	in := NewIntake(yes, out.emit)
	tg := &fakeMessenger{posts: []Post{{ID: 1, At: time.Now(), Text: "x"}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := in.Run(ctx, tg, []Source{{Name: "public"}}); !errors.Is(err, context.Canceled) || len(out.posts) != 1 {
		t.Fatalf("err = %v, posts = %d", err, len(out.posts))
	}
}

func TestIDRingEvictsOldest(t *testing.T) {
	r := &idRing{ids: make(map[int]struct{})}
	for id := 1; id <= seenPerChannel; id++ {
		if !r.add(id) {
			t.Fatalf("id %d reported as seen", id)
		}
	}
	if r.add(1) {
		t.Fatal("id 1 must still be remembered")
	}
	r.add(seenPerChannel + 1)
	if len(r.ids) != seenPerChannel || !r.add(1) {
		t.Fatalf("oldest id must be evicted, size = %d", len(r.ids))
	}
}

func BenchmarkIntakeAccept(b *testing.B) {
	in := NewIntake(yes, func(Post) {})
	now := time.Now()
	b.ReportAllocs()
	id := 0
	for b.Loop() {
		id++
		in.Accept(Post{ID: id, At: now, Channel: "a", Text: "Шахеды курсом на Одессу"})
	}
}
