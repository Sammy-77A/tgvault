package tgclient

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tgerr"
)

func quickRetries(t *testing.T) *Client {
	t.Helper()
	old := retryDelay
	retryDelay = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { retryDelay = old })
	return &Client{online: true}
}

func TestRetryCarriesOnThroughShortTrouble(t *testing.T) {
	c := quickRetries(t)
	calls := 0
	err := c.retry(context.Background(), "test", func() error {
		calls++
		if calls < 5 {
			return errors.New("rpcDoRequest: retryUntilAck: retry limit reached after 5 attempts")
		}
		return nil
	})
	if err != nil || calls != 5 {
		t.Fatalf("calls %d, err %v", calls, err)
	}
}

func TestRetryGivesUpOnDefiniteAnswersAndLongTrouble(t *testing.T) {
	c := quickRetries(t)
	gone := 0
	if err := c.retry(context.Background(), "t", func() error { gone++; return errGone }); !errors.Is(err, errGone) || gone != 1 {
		t.Errorf("a deleted file must not be retried: %d tries, %v", gone, err)
	}
	denied := 0
	rpc := tgerr.New(400, "CHANNEL_PRIVATE")
	if err := c.retry(context.Background(), "t", func() error { denied++; return rpc }); err == nil || denied != 1 {
		t.Errorf("a definite Telegram answer must not be retried: %d tries", denied)
	}
	always := 0
	if err := c.retry(context.Background(), "t", func() error { always++; return errors.New("i/o timeout") }); err == nil || always != blockTries {
		t.Errorf("long trouble should end after %d tries, got %d (%v)", blockTries, always, err)
	}
	server := 0
	_ = c.retry(context.Background(), "t", func() error {
		server++
		if server < 3 {
			return tgerr.New(500, "RPC_CALL_FAIL")
		}
		return nil
	})
	if server != 3 {
		t.Errorf("a Telegram server hiccup should be retried, got %d tries", server)
	}
}

func TestRetryStopsWhenCancelled(t *testing.T) {
	c := quickRetries(t)
	ctx, cancel := context.WithCancel(context.Background())
	tries := 0
	err := c.retry(ctx, "t", func() error { tries++; cancel(); return errors.New("i/o timeout") })
	if !errors.Is(err, context.Canceled) || tries != 1 {
		t.Fatalf("%d tries, %v", tries, err)
	}
}

func TestConnectionErrorsAreExplainedInPlainWords(t *testing.T) {
	for _, msg := range []string{
		"rpcDoRequest: retryUntilAck: retry limit reached after 5 attempts",
		"upload part 472: waitSession: context deadline exceeded",
		"read tcp 10.0.0.2:5->149.154.167.91:443: i/o timeout",
	} {
		got := friendly(errors.New(msg))
		if !strings.Contains(got, "lost the connection to Telegram") || strings.Contains(got, "rpcDoRequest") {
			t.Errorf("%q became %q", msg, got)
		}
	}
	if got := friendly(tgerr.New(400, "CHANNEL_PRIVATE")); strings.Contains(got, "lost the connection") {
		t.Errorf("an answer from Telegram is not a connection problem: %q", got)
	}
}

// A first download failed outright with "rpc error code 503: Timeout". That
// is Telegram being slow, not a definite answer, so it must be retried.
func TestA503TimeoutIsRetriedNotFatal(t *testing.T) {
	c := quickRetries(t)
	for _, e := range []error{tgerr.New(503, "Timeout"), tgerr.New(503, "TIMEOUT"), tgerr.New(502, "Bad Gateway"), tgerr.New(504, "Gateway Timeout"), tgerr.New(500, "WORKER_BUSY_TOO_LONG_RETRY")} {
		calls := 0
		err := c.retry(context.Background(), "t", func() error {
			calls++
			if calls < 3 {
				return e
			}
			return nil
		})
		if err != nil || calls != 3 {
			t.Errorf("%v: %d tries, %v", e, calls, err)
		}
	}
	if transient(tgerr.New(400, "FILE_REFERENCE_EMPTY")) || transient(tgerr.New(401, "AUTH_KEY_UNREGISTERED")) {
		t.Error("definite answers stay fatal")
	}
}
