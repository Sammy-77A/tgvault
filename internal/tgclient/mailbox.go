package tgclient

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/gotd/td/tg"
)

// The movie app keeps its requests in the account's own Saved Messages: a
// private chat nobody else can see, so it needs no server. Each request is a
// short text message starting with a hashtag.

func (c *Client) selfAPI() (*tg.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status != StatusReady || c.api == nil {
		return nil, errors.New("not connected to Telegram yet")
	}
	return c.api, nil
}

// SendSelf posts a text message to Saved Messages and returns its ID.
func (c *Client) SendSelf(ctx context.Context, text string) (int, error) {
	api, err := c.selfAPI()
	if err != nil {
		return 0, err
	}
	rid := randInt64()
	upd, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
		Peer: &tg.InputPeerSelf{}, Message: text, RandomID: rid, Silent: true,
	})
	c.paced(err)
	if err != nil {
		return 0, err
	}
	id := messageIDFromUpdates(upd, rid)
	if id == 0 {
		return 0, errors.New("Telegram accepted the message but did not return its ID")
	}
	return id, nil
}

// SelfHistory calls fn, oldest first, for each Saved Messages text newer than
// minID that starts with prefix.
func (c *Client) SelfHistory(ctx context.Context, minID int, prefix string, fn func(id int, text string, date int64) error) error {
	api, err := c.selfAPI()
	if err != nil {
		return err
	}
	type item struct {
		id   int
		text string
		date int64
	}
	var found []item
	offset := 0
	for {
		res, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer: &tg.InputPeerSelf{}, OffsetID: offset, Limit: 100, MinID: minID,
		})
		if err != nil {
			return err
		}
		var msgs []tg.MessageClass
		switch r := res.(type) {
		case *tg.MessagesMessages:
			msgs = r.Messages
		case *tg.MessagesMessagesSlice:
			msgs = r.Messages
		case *tg.MessagesChannelMessages:
			msgs = r.Messages
		}
		if len(msgs) == 0 {
			break
		}
		for _, m := range msgs {
			if m.GetID() <= minID {
				continue
			}
			offset = m.GetID()
			if msg, ok := m.(*tg.Message); ok && strings.HasPrefix(msg.Message, prefix) {
				found = append(found, item{msg.ID, msg.Message, int64(msg.Date)})
			}
		}
		if len(msgs) < 100 {
			break
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].id < found[j].id })
	for _, it := range found {
		if err := fn(it.id, it.text, it.date); err != nil {
			return err
		}
	}
	return nil
}
