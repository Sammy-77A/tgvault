package backup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

// Requests: the phone asks for a movie or a season, the PC owner is told and
// files it, then marks it done. The messages live in the account's Saved
// Messages as short hashtagged texts, so no server is needed:
//
//	#tgmb-request {json}      the phone asks
//	#tgmb-fulfilled {"id":…}  the PC says it's in the library
//	#tgmb-declined {"id":…}   the PC says no

const (
	mailTag     = "#tgmb-"
	reqTag      = "#tgmb-request "
	fulfilTag   = "#tgmb-fulfilled "
	declineTag  = "#tgmb-declined "
	cfgTag      = "#tgmb-config "
	settingLast = "media.req_last"
)

// mailbox is what the requests need from Telegram (the real client has it).
type mailbox interface {
	SendSelf(ctx context.Context, text string) (int, error)
	SelfHistory(ctx context.Context, minID int, prefix string, fn func(id int, text string, date int64) error) error
}

// SendSelf and SelfHistory pass through, but a read-only engine may only post
// requests.
func (r readOnlyTG) SendSelf(ctx context.Context, text string) (int, error) {
	m, ok := r.telegram.(mailbox)
	if !ok {
		return 0, errors.New("requests aren't available on this connection")
	}
	if !strings.HasPrefix(text, reqTag) {
		return 0, errReadOnly
	}
	return m.SendSelf(ctx, text)
}

func (r readOnlyTG) SelfHistory(ctx context.Context, minID int, prefix string, fn func(int, string, int64) error) error {
	m, ok := r.telegram.(mailbox)
	if !ok {
		return errors.New("requests aren't available on this connection")
	}
	return m.SelfHistory(ctx, minID, prefix, fn)
}

// Request is a wish for a movie or for seasons of a show.
type Request struct {
	ID       string           `json:"id"`
	Kind     string           `json:"kind"` // "movie" or "show"
	TMDB     int              `json:"tmdb"`
	Title    string           `json:"title"`
	Year     int              `json:"year,omitempty"`
	Poster   string           `json:"poster,omitempty"`
	Seasons  []int            `json:"seasons,omitempty"`  // empty for a show means every season
	Episodes map[string][]int `json:"episodes,omitempty"` // season -> wanted episodes, when not the whole season
	From     string           `json:"from,omitempty"`
	At       int64            `json:"at"`

	// Filled in when listing.
	State string `json:"state,omitempty"` // requested, fulfilled or declined
	MsgID int    `json:"msgId,omitempty"`
}

func (e *Engine) mailbox() (mailbox, error) {
	m, ok := e.tg.(mailbox)
	if !ok {
		return nil, errors.New("requests aren't available on this connection")
	}
	return m, nil
}

func newReqID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// MakeRequest posts a request to Saved Messages.
func (e *Engine) MakeRequest(ctx context.Context, r Request) (Request, error) {
	if (r.Kind != "movie" && r.Kind != "show") || r.TMDB <= 0 || strings.TrimSpace(r.Title) == "" {
		return r, errors.New("that request is missing its title")
	}
	m, err := e.mailbox()
	if err != nil {
		return r, err
	}
	for _, old := range e.requestList() {
		if old.TMDB == r.TMDB && old.Kind == r.Kind && old.State == "requested" && sameWish(old, r) {
			return old, errors.New("you've already asked for that; it's waiting")
		}
	}
	r.ID, r.At, r.State, r.From = newReqID(), time.Now().Unix(), "", e.Device()
	body, _ := json.Marshal(r)
	id, err := m.SendSelf(ctx, reqTag+string(body))
	if err != nil {
		return r, err
	}
	r.MsgID, r.State = id, "requested"
	_, err = e.st.MediaReqPut(store.MediaRequest{ID: r.ID, MsgID: id, State: "requested", Body: string(body), At: r.At})
	return r, err
}

func sameWish(a, b Request) bool {
	x, y := append([]int(nil), a.Seasons...), append([]int(nil), b.Seasons...)
	sort.Ints(x)
	sort.Ints(y)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return len(a.Episodes) == 0 && len(b.Episodes) == 0
}

func (e *Engine) requestList() []Request {
	rows, _ := e.st.MediaReqs()
	out := make([]Request, 0, len(rows))
	for _, row := range rows {
		var r Request
		if json.Unmarshal([]byte(row.Body), &r) != nil {
			continue
		}
		r.ID, r.State, r.MsgID = row.ID, row.State, row.MsgID
		if r.At == 0 {
			r.At = row.At
		}
		out = append(out, r)
	}
	return out
}

// Requests lists every request, newest first.
func (e *Engine) Requests() []Request { return e.requestList() }

// SyncRequests reads Saved Messages for new requests and for answers to them.
// It returns the requests seen for the first time.
func (e *Engine) SyncRequests(ctx context.Context) ([]Request, error) {
	m, err := e.mailbox()
	if err != nil {
		return nil, err
	}
	last, _ := strconv.Atoi(e.st.Get(settingLast))
	var fresh []Request
	top := last
	err = m.SelfHistory(ctx, last, mailTag, func(id int, text string, date int64) error {
		if id > top {
			top = id
		}
		switch {
		case strings.HasPrefix(text, reqTag):
			var r Request
			if json.Unmarshal([]byte(strings.TrimPrefix(text, reqTag)), &r) != nil || r.ID == "" {
				return nil
			}
			if r.At == 0 {
				r.At = date
			}
			isNew, err := e.st.MediaReqPut(store.MediaRequest{ID: r.ID, MsgID: id, State: "requested", Body: strings.TrimPrefix(text, reqTag), At: r.At})
			if err != nil {
				return err
			}
			if isNew {
				r.State, r.MsgID = "requested", id
				fresh = append(fresh, r)
			}
		case strings.HasPrefix(text, cfgTag):
			// the PC's keys for the phone app (the last one wins); the PC itself ignores its own
			if !e.readOnly {
				return nil
			}
			var c struct {
				TMDB string `json:"tmdb"`
				Subs string `json:"subs"`
			}
			if json.Unmarshal([]byte(strings.TrimPrefix(text, cfgTag)), &c) == nil {
				if c.TMDB != "" {
					_ = e.st.Set(SettingTMDBKey, c.TMDB)
				}
				if c.Subs != "" {
					_ = e.st.Set(SettingSubtitleKey, c.Subs)
				}
			}
		case strings.HasPrefix(text, fulfilTag), strings.HasPrefix(text, declineTag):
			state, tag := "fulfilled", fulfilTag
			if strings.HasPrefix(text, declineTag) {
				state, tag = "declined", declineTag
			}
			var a struct {
				ID string `json:"id"`
			}
			if json.Unmarshal([]byte(strings.TrimPrefix(text, tag)), &a) == nil && a.ID != "" {
				if _, known := e.st.MediaReq(a.ID); known {
					return e.st.MediaReqState(a.ID, state)
				}
				// the answer arrived before its request; keep it so the request, when it
				// shows up, starts out answered
				_, err := e.st.MediaReqPut(store.MediaRequest{ID: a.ID, State: state, Body: "", At: date})
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fresh, err
	}
	if top > last {
		_ = e.st.Set(settingLast, strconv.Itoa(top))
	}
	return fresh, nil
}

// PublishConfig shares the service keys with the phone app through Saved
// Messages (a private chat), so the phone app needs no key of its own. It posts
// only when the keys changed.
func (e *Engine) PublishConfig(ctx context.Context) error {
	if e.readOnly {
		return nil
	}
	cfg := map[string]string{}
	if k := strings.TrimSpace(e.st.Get(SettingTMDBKey)); k != "" {
		cfg["tmdb"] = k
	}
	if k := strings.TrimSpace(e.st.Get(SettingSubtitleKey)); k != "" {
		cfg["subs"] = k
	}
	if len(cfg) == 0 {
		return nil
	}
	body, _ := json.Marshal(cfg)
	sum := sha256.Sum256(body)
	h := hex.EncodeToString(sum[:])
	if e.st.Get("media.cfg_pub") == h {
		return nil
	}
	m, err := e.mailbox()
	if err != nil {
		return err
	}
	if _, err := m.SendSelf(ctx, cfgTag+string(body)); err != nil {
		return err
	}
	return e.st.Set("media.cfg_pub", h)
}

// Answer tells everyone a request is filed (or won't be).
func (e *Engine) Answer(ctx context.Context, id string, fulfilled bool) error {
	row, ok := e.st.MediaReq(id)
	if !ok {
		return errors.New("no such request")
	}
	m, err := e.mailbox()
	if err != nil {
		return err
	}
	tag, state := fulfilTag, "fulfilled"
	if !fulfilled {
		tag, state = declineTag, "declined"
	}
	body, _ := json.Marshal(map[string]string{"id": row.ID})
	if _, err := m.SendSelf(ctx, tag+string(body)); err != nil {
		return err
	}
	return e.st.MediaReqState(id, state)
}

// WatchRequests checks for new requests until ctx ends, calling onNew with
// the ones not seen before (the PC shows a notification for them).
func (e *Engine) WatchRequests(ctx context.Context, every time.Duration, onNew func([]Request)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if e.tg.Info().Status != tgclient.StatusReady {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_ = e.PublishConfig(cctx)
		fresh, err := e.SyncRequests(cctx)
		cancel()
		if err == nil && len(fresh) > 0 && onNew != nil {
			onNew(fresh)
		}
	}
}
