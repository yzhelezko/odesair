package main

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

type fakeAPI struct {
	channels map[string]*tg.Channel
	joinErr  error
	joined   []int64
	history  []tg.MessageClass
	sent     []*tg.MessagesSendMessageRequest
}

func (f *fakeAPI) ContactsResolveUsername(_ context.Context, username string) (*tg.ContactsResolvedPeer, error) {
	ch, ok := f.channels[username]
	if !ok {
		return nil, errors.New("USERNAME_NOT_OCCUPIED")
	}
	return &tg.ContactsResolvedPeer{Chats: []tg.ChatClass{ch}}, nil
}

func (f *fakeAPI) ChannelsJoinChannel(_ context.Context, channel tg.InputChannelClass) (tg.UpdatesClass, error) {
	if f.joinErr != nil {
		return nil, f.joinErr
	}
	f.joined = append(f.joined, channel.(*tg.InputChannel).ChannelID)
	return &tg.Updates{}, nil
}

func (f *fakeAPI) MessagesGetHistory(context.Context, *tg.MessagesGetHistoryRequest) (tg.MessagesMessagesClass, error) {
	return &tg.MessagesChannelMessages{Messages: f.history}, nil
}

func (f *fakeAPI) MessagesSendMessage(_ context.Context, req *tg.MessagesSendMessageRequest) (tg.UpdatesClass, error) {
	f.sent = append(f.sent, req)
	return &tg.Updates{}, nil
}

func testTelegram(api *fakeAPI) *Telegram {
	t := newTelegram()
	t.api = api
	return t
}

// channel sets the access hash through its setter, as a decoded channel carries the flag.
func channel(id, hash int64, left bool) *tg.Channel {
	ch := &tg.Channel{ID: id, Left: left}
	ch.SetAccessHash(hash)
	return ch
}

func TestResolveJoinsOnlyChannelsNotJoinedYet(t *testing.T) {
	api := &fakeAPI{channels: map[string]*tg.Channel{
		"out":    channel(1, 11, true),
		"member": channel(2, 22, false),
		"public": channel(3, 33, true),
	}}
	tgc := testTelegram(api)

	sources, err := tgc.Resolve(context.Background(), "out", []string{"member", "public", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Source{{Name: "member", Push: true}, {Name: "public", Push: true}}
	if !slices.Equal(sources, want) {
		t.Fatalf("sources = %+v, want %+v", sources, want)
	}
	if !slices.Equal(api.joined, []int64{3}) {
		t.Fatalf("joined = %v: only the source the account has not joined may be joined", api.joined)
	}
	if hash, ok, _ := tgc.GetChannelAccessHash(context.Background(), 0, 3); !ok || hash != 33 {
		t.Fatalf("access hash = %d, %v", hash, ok)
	}
}

func TestResolveKeepsPollingWhenJoinFails(t *testing.T) {
	api := &fakeAPI{
		channels: map[string]*tg.Channel{"out": {ID: 1}, "public": {ID: 3, Left: true}},
		joinErr:  errors.New("INVITE_REQUEST_SENT"),
	}
	sources, err := testTelegram(api).Resolve(context.Background(), "out", []string{"public"})
	if err != nil || !slices.Equal(sources, []Source{{Name: "public"}}) {
		t.Fatalf("sources = %+v, err = %v", sources, err)
	}
}

func TestResolveFailsWithoutOutputOrSources(t *testing.T) {
	api := &fakeAPI{channels: map[string]*tg.Channel{"out": {ID: 1}}}
	if _, err := testTelegram(api).Resolve(context.Background(), "nope", []string{"out"}); err == nil {
		t.Fatal("unresolved output channel must fail")
	}
	if _, err := testTelegram(api).Resolve(context.Background(), "out", []string{"nope"}); err == nil {
		t.Fatal("no resolved source must fail")
	}
}

func TestRecentReturnsOldestFirstAndSkipsServiceMessages(t *testing.T) {
	now := int(time.Now().Unix())
	api := &fakeAPI{
		channels: map[string]*tg.Channel{"out": {ID: 1}, "src": {ID: 2}},
		history: []tg.MessageClass{
			&tg.Message{ID: 30, Date: now, Message: "newest"},
			&tg.MessageService{ID: 20},
			&tg.Message{ID: 10, Date: now - 60, Message: "oldest"},
		},
	}
	tgc := testTelegram(api)
	if _, err := tgc.Resolve(context.Background(), "out", []string{"src"}); err != nil {
		t.Fatal(err)
	}

	posts, err := tgc.Recent(context.Background(), "src", 10)
	if err != nil || len(posts) != 2 || posts[0].ID != 10 || posts[1].Text != "newest" || posts[0].Channel != "src" {
		t.Fatalf("posts = %+v, err = %v", posts, err)
	}
	if got := posts[1].At.Unix(); got != int64(now) {
		t.Fatalf("time = %d, want %d", got, now)
	}
	if _, err := tgc.Recent(context.Background(), "other", 10); err == nil {
		t.Fatal("unknown channel must fail")
	}
}

func TestSendPostsToTheOutputChannel(t *testing.T) {
	api := &fakeAPI{channels: map[string]*tg.Channel{"out": channel(1, 11, false), "src": channel(2, 22, false)}}
	tgc := testTelegram(api)
	if _, err := tgc.Resolve(context.Background(), "out", []string{"src"}); err != nil {
		t.Fatal(err)
	}

	if err := tgc.Send(context.Background(), "✅ отбой", true); err != nil {
		t.Fatal(err)
	}
	req := api.sent[0]
	peer := req.Peer.(*tg.InputPeerChannel)
	if peer.ChannelID != 1 || peer.AccessHash != 11 || req.Message != "✅ отбой" || !req.Silent || req.RandomID == 0 {
		t.Fatalf("request = %+v", req)
	}
}

func TestHandlerForwardsOnlySourceChannels(t *testing.T) {
	api := &fakeAPI{channels: map[string]*tg.Channel{"out": {ID: 1}, "src": {ID: 2}}}
	tgc := testTelegram(api)
	if _, err := tgc.Resolve(context.Background(), "out", []string{"src"}); err != nil {
		t.Fatal(err)
	}
	var out sink
	handle := tgc.Handler(out.emit)
	push := func(m tg.MessageClass) {
		handle(context.Background(), tg.Entities{}, &tg.UpdateNewChannelMessage{Message: m})
	}

	push(&tg.Message{ID: 5, Message: "from source", PeerID: &tg.PeerChannel{ChannelID: 2}})
	push(&tg.Message{ID: 6, Message: "from output", PeerID: &tg.PeerChannel{ChannelID: 1}})
	push(&tg.Message{ID: 7, Message: "elsewhere", PeerID: &tg.PeerChannel{ChannelID: 99}})
	push(&tg.MessageService{ID: 8, PeerID: &tg.PeerChannel{ChannelID: 2}})

	if len(out.posts) != 1 || out.posts[0].ID != 5 || out.posts[0].Channel != "src" {
		t.Fatalf("posts = %+v", out.posts)
	}
}
