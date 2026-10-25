package tgclient

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/telegram/query/messages"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"golang.org/x/sync/errgroup"

	"github.com/Sammy-77A/tgvault/internal/store"
)

const (
	partSize      = 512 * 1024
	bigFileLimit  = 10 * 1024 * 1024
	uploadThreads = 4
)

// ErrPartsExpired means Telegram no longer has the parts of a resumed upload.
var ErrPartsExpired = errors.New("upload parts expired")

func randInt64() int64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return int64(binary.LittleEndian.Uint64(b[:]) & (1<<63 - 1))
}

// UploadRequest describes one file to store.
type UploadRequest struct {
	Path     string // local file to read
	Name     string // file name shown in Telegram
	Size     int64
	Caption  string
	Resume   *store.Resume           // saved progress for big files, may be nil
	Hash     string                  // content hash, used to validate resume
	OnResume func(store.Resume)      // called as big-file progress is made
	Progress func(done, total int64) // optional
}

// Upload stores a file in the channel and returns its message ID.
func (c *Client) Upload(ctx context.Context, req UploadRequest) (int, error) {
	api, ch, err := c.conn()
	if err != nil {
		return 0, err
	}
	f, err := os.Open(req.Path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	input, err := uploadParts(ctx, api, f, req)
	if err != nil {
		return 0, err
	}

	mt := mime.TypeByExtension(filepath.Ext(req.Name))
	if mt == "" {
		mt = "application/octet-stream"
	}
	if err := c.pace(ctx); err != nil {
		return 0, err
	}
	randomID := randInt64()
	upd, err := api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Peer: &tg.InputPeerChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash},
		Media: &tg.InputMediaUploadedDocument{
			ForceFile:  true,
			File:       input,
			MimeType:   mt,
			Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: req.Name}},
		},
		Message:  req.Caption,
		RandomID: randomID,
		Silent:   true,
	})
	c.paced(err)
	if err != nil {
		if isPartMissing(err) {
			return 0, ErrPartsExpired
		}
		return 0, err
	}
	id := messageIDFromUpdates(upd, randomID)
	if id == 0 {
		return 0, errors.New("Telegram accepted the file but did not return its message ID")
	}
	return id, nil
}

// Telegram limits how often an account may post. Spacing posts out avoids
// hitting the limit (which costs a long forced pause) in the first place.
const (
	minSendGap = 3 * time.Second
	maxSendGap = 30 * time.Second
)

// pace waits until the next post is allowed.
func (c *Client) pace(ctx context.Context) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.sendGap == 0 {
		c.sendGap = minSendGap
	}
	if wait := time.Until(c.lastSend.Add(c.sendGap)); wait > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	c.lastSend = time.Now()
	return nil
}

// paced adapts the spacing: slower after a rate limit, slowly faster after successes.
func (c *Client) paced(err error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if _, flood := tgerr.AsFloodWait(err); flood {
		c.sendGap = min(c.sendGap*2, maxSendGap)
		c.sendOK = 0
		return
	}
	if err == nil {
		c.sendOK++
		if c.sendOK >= 25 && c.sendGap > minSendGap {
			c.sendGap = max(c.sendGap*9/10, minSendGap)
			c.sendOK = 0
		}
	}
}

// isPartMissing matches FILE_PART_<n>_MISSING and FILE_PARTS_INVALID.
func isPartMissing(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "FILE_PARTS_INVALID") ||
		(strings.Contains(msg, "FILE_PART_") && strings.Contains(msg, "_MISSING"))
}

func uploadParts(ctx context.Context, api *tg.Client, f *os.File, req UploadRequest) (tg.InputFileClass, error) {
	total := int((req.Size + partSize - 1) / partSize)
	if total == 0 {
		return nil, errors.New("empty file")
	}
	big := req.Size > bigFileLimit

	fileID := randInt64()
	start := 0
	if big && req.Resume != nil && req.Resume.Hash == req.Hash && req.Resume.PartSize == partSize && req.Resume.DoneParts < total {
		fileID, start = req.Resume.FileID, req.Resume.DoneParts
	}

	var uploaded atomic.Int64
	uploaded.Store(int64(start) * partSize)
	var mu sync.Mutex
	done := make([]bool, total)
	contiguous := start
	lastSaved := time.Now()

	parts := make(chan int)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		defer close(parts)
		for p := start; p < total; p++ {
			select {
			case parts <- p:
			case <-gctx.Done():
				return gctx.Err()
			}
		}
		return nil
	})
	for w := 0; w < uploadThreads; w++ {
		g.Go(func() error {
			buf := make([]byte, partSize)
			for p := range parts {
				n, err := f.ReadAt(buf, int64(p)*partSize)
				if err != nil && !errors.Is(err, io.EOF) {
					return err
				}
				data := buf[:n]
				if err := savePart(gctx, api, big, fileID, p, total, data); err != nil {
					return err
				}
				uploaded.Add(int64(n))
				if req.Progress != nil {
					req.Progress(uploaded.Load(), req.Size)
				}
				if big {
					mu.Lock()
					done[p] = true
					for contiguous < total && done[contiguous] {
						contiguous++
					}
					if req.OnResume != nil && time.Since(lastSaved) > 3*time.Second {
						lastSaved = time.Now()
						req.OnResume(store.Resume{Hash: req.Hash, FileID: fileID, PartSize: partSize, DoneParts: contiguous})
					}
					mu.Unlock()
				}
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		if big && req.OnResume != nil {
			mu.Lock()
			req.OnResume(store.Resume{Hash: req.Hash, FileID: fileID, PartSize: partSize, DoneParts: contiguous})
			mu.Unlock()
		}
		return nil, err
	}
	if big {
		return &tg.InputFileBig{ID: fileID, Parts: total, Name: req.Name}, nil
	}
	return &tg.InputFile{ID: fileID, Parts: total, Name: req.Name}, nil
}

func savePart(ctx context.Context, api *tg.Client, big bool, fileID int64, part, total int, data []byte) error {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * 2 * time.Second):
			}
		}
		pctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		var err error
		if big {
			_, err = api.UploadSaveBigFilePart(pctx, &tg.UploadSaveBigFilePartRequest{
				FileID: fileID, FilePart: part, FileTotalParts: total, Bytes: data,
			})
		} else {
			_, err = api.UploadSaveFilePart(pctx, &tg.UploadSaveFilePartRequest{
				FileID: fileID, FilePart: part, Bytes: data,
			})
		}
		cancel()
		if err == nil {
			return nil
		}
		if _, flood := tgerr.AsFloodWait(err); flood || ctx.Err() != nil {
			return err // the scheduler handles long waits
		}
		lastErr = err
	}
	return fmt.Errorf("upload part %d: %w", part, lastErr)
}

func messageIDFromUpdates(u tg.UpdatesClass, randomID int64) int {
	var list []tg.UpdateClass
	switch v := u.(type) {
	case *tg.Updates:
		list = v.Updates
	case *tg.UpdatesCombined:
		list = v.Updates
	case *tg.UpdateShortSentMessage:
		return v.ID
	}
	fallback := 0
	for _, up := range list {
		switch x := up.(type) {
		case *tg.UpdateMessageID:
			if x.RandomID == randomID {
				return x.ID
			}
		case *tg.UpdateNewChannelMessage:
			if m, ok := x.Message.(*tg.Message); ok {
				fallback = m.ID
			}
		}
	}
	return fallback
}

// RemoteFile is a stored document.
type RemoteFile struct {
	MsgID   int
	Caption string
	Size    int64
	Date    int
	doc     *tg.Document
}

// getDocs fetches messages by ID; missing or non-file messages are left out.
func (c *Client) getDocs(ctx context.Context, ids []int) (map[int]RemoteFile, error) {
	api, ch, err := c.conn()
	if err != nil {
		return nil, err
	}
	out := map[int]RemoteFile{}
	for i := 0; i < len(ids); i += 100 {
		end := min(i+100, len(ids))
		var in []tg.InputMessageClass
		for _, id := range ids[i:end] {
			in = append(in, &tg.InputMessageID{ID: id})
		}
		res, err := api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{Channel: ch, ID: in})
		if err != nil {
			return nil, err
		}
		mm, ok := res.(*tg.MessagesChannelMessages)
		if !ok {
			return nil, fmt.Errorf("unexpected reply %T", res)
		}
		for _, m := range mm.Messages {
			if rf, ok := remoteFile(m); ok {
				out[rf.MsgID] = rf
			}
		}
	}
	return out, nil
}

func remoteFile(m any) (RemoteFile, bool) {
	msg, ok := m.(*tg.Message)
	if !ok {
		return RemoteFile{}, false
	}
	media, ok := msg.Media.(*tg.MessageMediaDocument)
	if !ok {
		return RemoteFile{}, false
	}
	doc, ok := media.Document.(*tg.Document)
	if !ok {
		return RemoteFile{}, false
	}
	return RemoteFile{MsgID: msg.ID, Caption: msg.Message, Size: doc.Size, Date: msg.Date, doc: doc}, true
}

// Exists returns which of the message IDs still hold files.
func (c *Client) Exists(ctx context.Context, ids []int) (map[int]RemoteFile, error) {
	return c.getDocs(ctx, ids)
}

// Download writes a stored file to w.
func (c *Client) Download(ctx context.Context, msgID int, w io.Writer) error {
	docs, err := c.getDocs(ctx, []int{msgID})
	if err != nil {
		return err
	}
	rf, ok := docs[msgID]
	if !ok {
		return errors.New("this file is no longer in the Telegram channel")
	}
	api, _, err := c.conn()
	if err != nil {
		return err
	}
	loc := &tg.InputDocumentFileLocation{
		ID:            rf.doc.ID,
		AccessHash:    rf.doc.AccessHash,
		FileReference: rf.doc.FileReference,
	}
	_, err = downloader.NewDownloader().Download(api, loc).WithThreads(4).Stream(ctx, w)
	return err
}

// DeleteMessages removes messages (old versions) from the channel.
func (c *Client) DeleteMessages(ctx context.Context, ids []int) error {
	if len(ids) == 0 {
		return nil
	}
	api, ch, err := c.conn()
	if err != nil {
		return err
	}
	_, err = api.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{Channel: ch, ID: ids})
	return err
}

// ScanChannel calls fn for every file in the channel, newest first.
func (c *Client) ScanChannel(ctx context.Context, fn func(RemoteFile) error) error {
	api, ch, err := c.conn()
	if err != nil {
		return err
	}
	peer := &tg.InputPeerChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash}
	return query.Messages(api).GetHistory(peer).BatchSize(100).ForEach(ctx, func(ctx context.Context, e messages.Elem) error {
		if rf, ok := remoteFile(e.Msg); ok {
			return fn(rf)
		}
		return nil
	})
}

// ScanSince calls fn for every file in the channel newer than message minID,
// newest first. It's a cheap way to notice what other devices uploaded.
func (c *Client) ScanSince(ctx context.Context, minID int, fn func(RemoteFile) error) error {
	api, ch, err := c.conn()
	if err != nil {
		return err
	}
	peer := &tg.InputPeerChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash}
	offset := 0
	for {
		res, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer: peer, OffsetID: offset, Limit: 100, MinID: minID,
		})
		if err != nil {
			return err
		}
		var msgs []tg.MessageClass
		switch r := res.(type) {
		case *tg.MessagesChannelMessages:
			msgs = r.Messages
		case *tg.MessagesMessagesSlice:
			msgs = r.Messages
		case *tg.MessagesMessages:
			msgs = r.Messages
		}
		if len(msgs) == 0 {
			return nil
		}
		for _, m := range msgs {
			if m.GetID() <= minID {
				return nil
			}
			offset = m.GetID()
			if rf, ok := remoteFile(m); ok {
				if err := fn(rf); err != nil {
					return err
				}
			}
		}
		if len(msgs) < 100 {
			return nil
		}
	}
}

// EditCaption replaces a stored file's caption (used to move it in the backup).
func (c *Client) EditCaption(ctx context.Context, msgID int, caption string) error {
	api, ch, err := c.conn()
	if err != nil {
		return err
	}
	peer := &tg.InputPeerChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash}
	_, err = api.MessagesEditMessage(ctx, &tg.MessagesEditMessageRequest{Peer: peer, ID: msgID, Message: caption})
	if tgerr.Is(err, "MESSAGE_NOT_MODIFIED") {
		return nil
	}
	return err
}

// MessageLink is a t.me link that opens a stored file in the Telegram app
// (only for members of the private channel, which is just you).
func (c *Client) MessageLink(msgID int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.channel == nil {
		return ""
	}
	return fmt.Sprintf("https://t.me/c/%d/%d", c.channel.ChannelID, msgID)
}
