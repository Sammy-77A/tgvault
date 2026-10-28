package backup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Sammy-77A/tgvault/internal/store"
)

func TestConnectionDropsAreRetriedWithoutCounting(t *testing.T) {
	e, _ := newSplitEngine(t, newFakeTG())
	for _, msg := range []string{
		"rpcDoRequest: retryUntilAck: retry limit reached after 5 attempts",
		"upload part 0: client closed: context canceled",
		"rpcDoRequest: engine forcibly closed: context canceled",
		"write tcp 1.2.3.4:5->6.7.8.9:443: write: broken pipe",
	} {
		out := e.failure(context.Background(), errors.New(msg))
		if out.state != store.StateFailed || out.count || out.retry != 30*time.Second || !strings.Contains(out.note, "connection to Telegram dropped") {
			t.Errorf("%q: %+v; want a quiet 30 s retry that isn't counted", msg, out)
		}
	}
	// Our own cancellation (shutting down) isn't a dropped connection.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out := e.failure(ctx, errors.New("context canceled")); strings.Contains(out.note, "dropped") {
		t.Errorf("a cancelled context was taken for a dropped connection: %+v", out)
	}
	// A real problem still counts.
	if out := e.failure(context.Background(), errors.New("FILE_PARTS_INVALID")); !out.count {
		t.Errorf("a real error must count toward the backoff: %+v", out)
	}
}
