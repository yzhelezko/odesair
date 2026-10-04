package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
)

// Telegram implements Messenger and updates.ChannelAccessHasher.
type Telegram struct {
	api *tg.Client

	mu     sync.RWMutex
	out    *tg.InputPeerChannel
	peers  map[string]*tg.InputPeerChannel
	names  map[int64]string
	hashes map[int64]int64
}

func newTelegram() *Telegram {
	return &Telegram{
		peers:  make(map[string]*tg.InputPeerChannel),
		names:  make(map[int64]string),
		hashes: make(map[int64]int64),
	}
}

func authenticate(ctx context.Context, client *telegram.Client, cfg Config) error {
	code := auth.CodeAuthenticatorFunc(func(context.Context, *tg.AuthSentCode) (string, error) {
		fmt.Print("Enter code: ")
		var code string
		_, err := fmt.Scan(&code)
		return code, err
	})
	flow := auth.NewFlow(auth.Constant(cfg.Phone, cfg.Password, code), auth.SendCodeOptions{})
	return client.Auth().IfNecessary(ctx, flow)
}

func (t *Telegram) resolve(ctx context.Context, username string) (*tg.Channel, error) {
	res, err := t.api.ContactsResolveUsername(ctx, username)
	if err != nil {
		return nil, err
	}
	for _, chat := range res.Chats {
		if ch, ok := chat.(*tg.Channel); ok {
			return ch, nil
		}
	}
	return nil, errors.New("not a channel")
}

// Resolve caches peers once at startup and returns the sources that resolved.
func (t *Telegram) Resolve(ctx context.Context, out string, sources []string) ([]Source, error) {
	ch, err := t.resolve(ctx, out)
	if err != nil {
		return nil, fmt.Errorf("resolve output channel %s: %w", out, err)
	}
	t.mu.Lock()
	t.out = ch.AsInputPeer()
	t.peers[out] = t.out
	t.mu.Unlock()

	var resolved []Source
	for _, name := range sources {
		ch, err := t.resolve(ctx, name)
		if err != nil {
			slog.Error("source channel skipped", "channel", name, "err", err)
			continue
		}
		t.mu.Lock()
		t.peers[name] = ch.AsInputPeer()
		t.names[ch.ID] = name
		t.hashes[ch.ID] = ch.AccessHash
		t.mu.Unlock()
		slog.Info("source channel", "channel", name, "push", !ch.Left)
		resolved = append(resolved, Source{Name: name, Push: !ch.Left})
	}
	if len(resolved) == 0 {
		return nil, errors.New("no source channel resolved")
	}
	return resolved, nil
}

func (t *Telegram) Handler(accept func(Post)) tg.NewChannelMessageHandler {
	return func(_ context.Context, _ tg.Entities, u *tg.UpdateNewChannelMessage) error {
		msg, ok := u.Message.(*tg.Message)
		if !ok {
			return nil
		}
		peer, ok := msg.PeerID.(*tg.PeerChannel)
		if !ok {
			return nil
		}
		t.mu.RLock()
		name, ok := t.names[peer.ChannelID]
		t.mu.RUnlock()
		if ok {
			accept(toPost(name, msg))
		}
		return nil
	}
}

func (t *Telegram) Send(ctx context.Context, text string, silent bool) error {
	t.mu.RLock()
	out := t.out
	t.mu.RUnlock()
	_, err := t.api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
		Peer:     out,
		Message:  text,
		RandomID: rand.Int64(),
		Silent:   silent,
	})
	return err
}

func (t *Telegram) Recent(ctx context.Context, channel string, limit int) ([]Post, error) {
	t.mu.RLock()
	peer := t.peers[channel]
	t.mu.RUnlock()
	if peer == nil {
		return nil, fmt.Errorf("unknown channel %q", channel)
	}
	res, err := t.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, Limit: limit})
	if err != nil {
		return nil, err
	}
	history, ok := res.AsModified()
	if !ok {
		return nil, fmt.Errorf("unexpected history response %T", res)
	}
	msgs := history.GetMessages()
	posts := make([]Post, 0, len(msgs))
	for _, m := range slices.Backward(msgs) {
		if msg, ok := m.(*tg.Message); ok {
			posts = append(posts, toPost(channel, msg))
		}
	}
	return posts, nil
}

func toPost(channel string, msg *tg.Message) Post {
	return Post{ID: msg.ID, At: time.Unix(int64(msg.Date), 0), Channel: channel, Text: msg.Message}
}

func (t *Telegram) SetChannelAccessHash(_ context.Context, _, channelID, accessHash int64) error {
	t.mu.Lock()
	t.hashes[channelID] = accessHash
	t.mu.Unlock()
	return nil
}

func (t *Telegram) GetChannelAccessHash(_ context.Context, _, channelID int64) (int64, bool, error) {
	t.mu.RLock()
	hash, ok := t.hashes[channelID]
	t.mu.RUnlock()
	return hash, ok, nil
}
