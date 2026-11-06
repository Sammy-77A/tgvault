package tgclient

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Sammy-77A/tgvault/internal/store"
)

// With no internet the app opened on a "connecting to Telegram" screen,
// because the account's name was only known after connecting. It is
// remembered now, so a logged-in device opens as logged in.
func TestALoggedInDeviceOpensWithoutAConnection(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.Set(keyAPIID, "123")
	st.Set(keyAPIHash, "abc")
	st.Set(keyAccount, "Sam (@sam)")

	c := New(st, dir, "test")
	if in := c.Info(); in.Account != "" {
		t.Fatalf("no session file yet, so nobody is logged in: %+v", in)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	c = New(st, dir, "test")
	in := c.Info()
	if in.Account != "Sam (@sam)" || in.Status != StatusConnecting {
		t.Errorf("should open as logged in, connecting in the background: %+v", in)
	}
}
