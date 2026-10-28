package tgclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tgerr"
)

type invokerFunc func(ctx context.Context, input bin.Encoder, output bin.Decoder) error

func (f invokerFunc) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	return f(ctx, input, output)
}

func staleTestClient(t *testing.T) *Client {
	t.Helper()
	old := staleDelay
	staleDelay = time.Millisecond
	t.Cleanup(func() { staleDelay = old })
	return &Client{online: true}
}

var brokenPipe = fmt.Errorf("rpcDoRequest: retryUntilAck: send: write: write intermediate: write tcp 192.168.137.87:38734->149.154.167.91:443: %w",
	&net.OpError{Op: "write", Err: syscall.EPIPE})

func TestStaleConn(t *testing.T) {
	for _, err := range []error{
		brokenPipe,
		errors.New("write tcp 1.2.3.4:5->6.7.8.9:443: write: broken pipe"),
		errors.New("wsasend: An established connection was aborted by the software in your host machine."),
		fmt.Errorf("read: %w", syscall.ECONNRESET),
		errors.New("use of closed network connection"),
	} {
		if !staleConn(err) {
			t.Errorf("%v should count as a stale connection", err)
		}
	}
	for _, err := range []error{
		nil,
		context.Canceled,
		fmt.Errorf("rpc: %w", context.DeadlineExceeded),
		errors.New("FILE_PART_INVALID"),
		tgerr.New(400, "CHANNEL_INVALID"),
	} {
		if staleConn(err) {
			t.Errorf("%v must not count as a stale connection", err)
		}
	}
}

func TestRetryStaleSendsAgain(t *testing.T) {
	c := staleTestClient(t)
	calls := 0
	inv := c.retryStale(invokerFunc(func(context.Context, bin.Encoder, bin.Decoder) error {
		calls++
		if calls < 3 {
			return brokenPipe
		}
		return nil
	}))
	if err := inv.Invoke(context.Background(), nil, nil); err != nil || calls != 3 {
		t.Fatalf("calls=%d err=%v; want success on the third try", calls, err)
	}
}

func TestRetryStaleGivesUp(t *testing.T) {
	c := staleTestClient(t)
	calls := 0
	inv := c.retryStale(invokerFunc(func(context.Context, bin.Encoder, bin.Decoder) error { calls++; return brokenPipe }))
	if err := inv.Invoke(context.Background(), nil, nil); !errors.Is(err, syscall.EPIPE) || calls != staleRetries+1 {
		t.Fatalf("calls=%d err=%v; want %d tries then the error", calls, err, staleRetries+1)
	}
}

func TestRetryStaleLeavesOtherErrorsAlone(t *testing.T) {
	c := staleTestClient(t)
	calls := 0
	want := tgerr.New(400, "CHANNEL_INVALID")
	inv := c.retryStale(invokerFunc(func(context.Context, bin.Encoder, bin.Decoder) error { calls++; return want }))
	if err := inv.Invoke(context.Background(), nil, nil); err != want || calls != 1 {
		t.Fatalf("calls=%d err=%v; a reply from Telegram must not be retried", calls, err)
	}
}

func TestRetryStaleStopsWhenCancelled(t *testing.T) {
	c := staleTestClient(t)
	staleDelay = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	inv := c.retryStale(invokerFunc(func(context.Context, bin.Encoder, bin.Decoder) error { calls++; cancel(); return brokenPipe }))
	done := make(chan error, 1)
	go func() { done <- inv.Invoke(ctx, nil, nil) }()
	select {
	case err := <-done:
		if err == nil || calls != 1 {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("kept waiting after the request was cancelled")
	}
}
