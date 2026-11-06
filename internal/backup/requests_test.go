package backup

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sammy-77A/tgvault/internal/store"
)

type selfMsg struct {
	id   int
	text string
	date int64
}

// SendSelf and SelfHistory make fakeTG a mailbox: Saved Messages in memory.
func (f *fakeTG) SendSelf(ctx context.Context, text string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	f.self = append(f.self, selfMsg{f.next, text, time.Now().Unix()})
	return f.next, nil
}

func (f *fakeTG) SelfHistory(ctx context.Context, minID int, prefix string, fn func(int, string, int64) error) error {
	f.mu.Lock()
	var list []selfMsg
	for _, m := range f.self {
		if m.id > minID && strings.HasPrefix(m.text, prefix) {
			list = append(list, m)
		}
	}
	f.mu.Unlock()
	for _, m := range list {
		if err := fn(m.id, m.text, m.date); err != nil {
			return err
		}
	}
	return nil
}

// phoneAndPC: two engines, two databases, one Telegram account.
func phoneAndPC(t *testing.T) (pc, phone *Engine, tg *fakeTG) {
	pc, _, tg, _, _ = moveRig(t)
	st2, err := store.Open(filepath.Join(t.TempDir(), "phone.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st2.Close() })
	phone = NewWith(st2, tg, t.TempDir())
	phone.ReadOnly()
	return pc, phone, tg
}

func TestRequestTravelsFromPhoneToPCAndBack(t *testing.T) {
	pc, phone, _ := phoneAndPC(t)
	ctx := context.Background()

	r, err := phone.MakeRequest(ctx, Request{Kind: "show", TMDB: 34307, Title: "Shameless", Year: 2011, Poster: "/p.jpg", Seasons: []int{1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	if r.ID == "" || r.State != "requested" {
		t.Fatalf("request: %+v", r)
	}
	if _, err := phone.MakeRequest(ctx, Request{Kind: "show", TMDB: 34307, Title: "Shameless", Seasons: []int{2, 1}}); err == nil {
		t.Error("the same wish twice should be refused while the first waits")
	}

	fresh, err := pc.SyncRequests(ctx)
	if err != nil || len(fresh) != 1 {
		t.Fatalf("PC should see one new request: %v %+v", err, fresh)
	}
	got := fresh[0]
	if got.Title != "Shameless" || got.Kind != "show" || len(got.Seasons) != 2 || got.Poster != "/p.jpg" || got.ID != r.ID {
		t.Fatalf("the PC read it wrong: %+v", got)
	}
	if again, _ := pc.SyncRequests(ctx); len(again) != 0 {
		t.Errorf("a request is announced once, got %+v", again)
	}

	if err := pc.Answer(ctx, r.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := phone.SyncRequests(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	for _, x := range phone.Requests() {
		if x.ID == r.ID {
			state = x.State
		}
	}
	if state != "fulfilled" {
		t.Fatalf("the phone should see it fulfilled, got %q", state)
	}
	// and it can be asked for again once answered
	if _, err := phone.MakeRequest(ctx, Request{Kind: "show", TMDB: 34307, Title: "Shameless", Seasons: []int{1, 2}}); err != nil {
		t.Errorf("a fulfilled request may be repeated: %v", err)
	}
}

func TestDeclinedAndReadOnlyRules(t *testing.T) {
	pc, phone, tg := phoneAndPC(t)
	ctx := context.Background()
	r, _ := phone.MakeRequest(ctx, Request{Kind: "movie", TMDB: 14160, Title: "Up", Year: 2009})
	pc.SyncRequests(ctx)
	if err := pc.Answer(ctx, r.ID, false); err != nil {
		t.Fatal(err)
	}
	phone.SyncRequests(ctx)
	if got := phone.Requests()[0].State; got != "declined" {
		t.Errorf("want declined, got %s", got)
	}
	// the phone may post requests and nothing else
	m := phone.tg.(mailbox)
	if _, err := m.SendSelf(ctx, fulfilTag+`{"id":"x"}`); err == nil {
		t.Error("a read-only engine must not post answers")
	}
	if _, err := m.SendSelf(ctx, "hello"); err == nil {
		t.Error("a read-only engine must not post arbitrary text")
	}
	if err := phone.Answer(ctx, r.ID, true); err == nil {
		t.Error("the phone can't answer requests")
	}
	if len(tg.self) != 2 { // the request and its decline, nothing the phone tried to sneak in
		t.Errorf("unexpected messages in Saved Messages: %d", len(tg.self))
	}
}

func TestBadRequestsAreRefused(t *testing.T) {
	_, phone, _ := phoneAndPC(t)
	for _, r := range []Request{{Kind: "show", Title: "x"}, {Kind: "game", TMDB: 1, Title: "x"}, {Kind: "movie", TMDB: 1}} {
		if _, err := phone.MakeRequest(context.Background(), r); err == nil {
			t.Errorf("should refuse %+v", r)
		}
	}
}

func TestPCSharesItsKeysWithThePhoneThroughSavedMessages(t *testing.T) {
	pc, phone, tg := phoneAndPC(t)
	ctx := context.Background()
	if err := pc.PublishConfig(ctx); err != nil || len(tg.self) != 0 {
		t.Fatalf("nothing to share yet: %v %d", err, len(tg.self))
	}
	pc.SetTMDBKey("tmdb-key-1")
	pc.st.Set(SettingSubtitleKey, "sub-key-1")
	if err := pc.PublishConfig(ctx); err != nil || len(tg.self) != 1 {
		t.Fatalf("publish: %v %d", err, len(tg.self))
	}
	pc.PublishConfig(ctx)
	if len(tg.self) != 1 {
		t.Errorf("an unchanged key isn't posted again: %d", len(tg.self))
	}
	if phone.HasTMDBKey() {
		t.Fatal("the phone starts without a key")
	}
	phone.SyncRequests(ctx)
	if !phone.HasTMDBKey() || phone.st.Get(SettingTMDBKey) != "tmdb-key-1" || phone.st.Get(SettingSubtitleKey) != "sub-key-1" {
		t.Errorf("the phone should have both keys: %q %q", phone.st.Get(SettingTMDBKey), phone.st.Get(SettingSubtitleKey))
	}
	pc.SetTMDBKey("tmdb-key-2")
	pc.PublishConfig(ctx)
	phone.SyncRequests(ctx)
	if phone.st.Get(SettingTMDBKey) != "tmdb-key-2" {
		t.Errorf("a changed key follows: %q", phone.st.Get(SettingTMDBKey))
	}
	// the PC reading its own posts must not overwrite anything
	pc.SetTMDBKey("tmdb-key-3")
	pc.SyncRequests(ctx)
	if pc.st.Get(SettingTMDBKey) != "tmdb-key-3" {
		t.Errorf("the PC keeps its own key: %q", pc.st.Get(SettingTMDBKey))
	}
	// and the phone can't post keys (or anything but requests)
	if _, err := phone.tg.(mailbox).SendSelf(ctx, cfgTag+`{"tmdb":"evil"}`); err == nil {
		t.Error("the phone must not post configuration")
	}
}
