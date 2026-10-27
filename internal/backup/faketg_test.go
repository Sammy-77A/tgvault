package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

// fakeTG is an in-memory Telegram channel for tests.
type fakeTG struct {
	mu         sync.Mutex
	next       int
	msgs       map[int]*fakeMsg
	calls      int // uploads attempted
	blockReads int
	thumbs     map[int][]byte // message ID -> the thumbnail Telegram keeps
	thumbCalls int

	// Hooks for simulating trouble; each may be nil.
	beforeUpload func(n int, req tgclient.UploadRequest) error // n counts from 1
	afterUpload  func(n int)
	badDownloads map[int]int   // message ID -> how many downloads return corrupt data
	editFails    map[int]error // message ID -> error from EditCaption
}

type fakeMsg struct {
	caption string
	data    []byte
	date    int
	name    string
}

func newFakeTG() *fakeTG {
	return &fakeTG{next: 100, msgs: map[int]*fakeMsg{}, badDownloads: map[int]int{}, editFails: map[int]error{}}
}

func (f *fakeTG) Info() tgclient.Info                 { return tgclient.Info{Status: tgclient.StatusReady} }
func (f *fakeTG) WaitReady(ctx context.Context) error { return nil }
func (f *fakeTG) MessageLink(id int) string           { return fmt.Sprintf("https://t.me/c/1/%d", id) }

func (f *fakeTG) Upload(ctx context.Context, req tgclient.UploadRequest) (int, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if f.beforeUpload != nil {
		if err := f.beforeUpload(n, req); err != nil {
			return 0, err
		}
	}
	file, err := os.Open(req.Path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	data := make([]byte, req.Size)
	if _, err := file.ReadAt(data, req.Offset); err != nil {
		return 0, err
	}
	if req.Progress != nil {
		req.Progress(req.Size, req.Size)
	}
	f.mu.Lock()
	f.next++
	id := f.next
	f.msgs[id] = &fakeMsg{caption: req.Caption, data: data, date: int(time.Now().Unix()), name: req.Name}
	f.mu.Unlock()
	if f.afterUpload != nil {
		f.afterUpload(n)
	}
	return id, nil
}

func (f *fakeTG) Download(ctx context.Context, id int, w io.Writer) error {
	f.mu.Lock()
	m, ok := f.msgs[id]
	var data []byte
	if ok {
		data = append([]byte(nil), m.data...)
		if f.badDownloads[id] > 0 {
			f.badDownloads[id]--
			data[len(data)/2] ^= 0xff
		}
	}
	f.mu.Unlock()
	if !ok {
		return errors.New("this file is no longer in the Telegram channel")
	}
	_, err := w.Write(data)
	return err
}

func (f *fakeTG) ReadBlock(ctx context.Context, id int, n int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blockReads++
	m, ok := f.msgs[id]
	if !ok {
		return nil, errors.New("this file is no longer in the Telegram channel")
	}
	start := n * tgclient.BlockSize
	if start >= int64(len(m.data)) {
		return nil, io.EOF
	}
	end := min(start+tgclient.BlockSize, int64(len(m.data)))
	return append([]byte(nil), m.data[start:end]...), nil
}

func (f *fakeTG) DeleteMessages(ctx context.Context, ids []int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		delete(f.msgs, id)
	}
	return nil
}

func (f *fakeTG) EditCaption(ctx context.Context, id int, caption string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.editFails[id]; err != nil {
		return err
	}
	if m, ok := f.msgs[id]; ok {
		m.caption = caption
		return nil
	}
	return errors.New("MESSAGE_ID_INVALID")
}

func (f *fakeTG) remote(id int) (tgclient.RemoteFile, bool) {
	m, ok := f.msgs[id]
	if !ok {
		return tgclient.RemoteFile{}, false
	}
	return tgclient.RemoteFile{MsgID: id, Caption: m.caption, Size: int64(len(m.data)), Date: m.date}, true
}

func (f *fakeTG) Exists(ctx context.Context, ids []int) (map[int]tgclient.RemoteFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int]tgclient.RemoteFile{}
	for _, id := range ids {
		if rf, ok := f.remote(id); ok {
			out[id] = rf
		}
	}
	return out, nil
}

func (f *fakeTG) ids(minID int, newestFirst bool) []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []int
	for id := range f.msgs {
		if id > minID {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	if newestFirst {
		for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
			ids[i], ids[j] = ids[j], ids[i]
		}
	}
	return ids
}

func (f *fakeTG) each(ids []int, fn func(tgclient.RemoteFile) error) error {
	for _, id := range ids {
		f.mu.Lock()
		rf, ok := f.remote(id)
		f.mu.Unlock()
		if !ok {
			continue
		}
		if err := fn(rf); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeTG) ScanChannel(ctx context.Context, fn func(tgclient.RemoteFile) error) error {
	return f.each(f.ids(0, true), fn)
}

func (f *fakeTG) ScanSince(ctx context.Context, minID int, fn func(tgclient.RemoteFile) error) error {
	return f.each(f.ids(minID, false), fn)
}

// count returns how many messages the channel holds.
func (f *fakeTG) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.msgs)
}

func (f *fakeTG) Thumbs(ctx context.Context, ids []int) (map[int][]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.thumbCalls++
	out := map[int][]byte{}
	for _, id := range ids {
		out[id] = f.thumbs[id]
	}
	return out, nil
}
