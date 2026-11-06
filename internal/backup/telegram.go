package backup

import (
	"context"
	"io"

	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

// telegram is what the engine needs from the Telegram connection. Tests
// replace it with an in-memory channel.
type telegram interface {
	Info() tgclient.Info
	WaitReady(ctx context.Context) error
	Upload(ctx context.Context, req tgclient.UploadRequest) (int, error)
	Download(ctx context.Context, msgID int, w io.Writer) error
	ReadBlock(ctx context.Context, msgID int, n int64) ([]byte, error)
	Thumbs(ctx context.Context, ids []int) (map[int][]byte, error)
	DeleteMessages(ctx context.Context, ids []int) error
	EditCaption(ctx context.Context, msgID int, caption string) error
	Exists(ctx context.Context, ids []int) (map[int]tgclient.RemoteFile, error)
	MessageLink(msgID int) string
	ScanChannel(ctx context.Context, fn func(tgclient.RemoteFile) error) error
	ScanSince(ctx context.Context, minID int, fn func(tgclient.RemoteFile) error) error
}

// Telegram is the connection an engine uploads to and reads from; the real
// one is tgclient.Client.
type Telegram = telegram

// NewWith creates an engine on any Telegram connection (tests use an in-memory one).
func NewWith(st *store.Store, tg Telegram, dataDir string) *Engine {
	e := newEngine(st, dataDir)
	e.tg = tg
	return e
}
