package friends

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sammy-77A/tgvault/internal/backup"
	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

// fakeTG is an in-memory Telegram channel.
type fakeTG struct {
	mu   sync.Mutex
	next int
	msgs map[int]*fakeMsg
}

type fakeMsg struct {
	caption string
	data    []byte
}

func (f *fakeTG) Info() tgclient.Info                 { return tgclient.Info{Status: tgclient.StatusReady} }
func (f *fakeTG) WaitReady(ctx context.Context) error { return nil }
func (f *fakeTG) MessageLink(id int) string           { return fmt.Sprint(id) }

func (f *fakeTG) Upload(ctx context.Context, req tgclient.UploadRequest) (int, error) {
	b, err := os.ReadFile(req.Path)
	if err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	f.msgs[f.next] = &fakeMsg{caption: req.Caption, data: b[req.Offset : req.Offset+req.Size]}
	return f.next, nil
}

func (f *fakeTG) Download(ctx context.Context, id int, w io.Writer) error {
	return f.DownloadFrom(ctx, id, 0, w)
}

func (f *fakeTG) DownloadFrom(ctx context.Context, id int, offset int64, w io.Writer) error {
	f.mu.Lock()
	m, ok := f.msgs[id]
	f.mu.Unlock()
	if !ok {
		return errors.New("gone")
	}
	_, err := w.Write(m.data)
	return err
}

func (f *fakeTG) ReadBlock(ctx context.Context, id int, n int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.msgs[id]
	if !ok {
		return nil, errors.New("gone")
	}
	start := n * tgclient.BlockSize
	if start >= int64(len(m.data)) {
		return nil, io.EOF
	}
	return m.data[start:min(start+tgclient.BlockSize, int64(len(m.data)))], nil
}

func (f *fakeTG) Thumbs(ctx context.Context, ids []int) (map[int][]byte, error) { return nil, nil }
func (f *fakeTG) EditCaption(ctx context.Context, id int, c string) error       { return nil }

func (f *fakeTG) DeleteMessages(ctx context.Context, ids []int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		delete(f.msgs, id)
	}
	return nil
}

func (f *fakeTG) Exists(ctx context.Context, ids []int) (map[int]tgclient.RemoteFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int]tgclient.RemoteFile{}
	for _, id := range ids {
		if m, ok := f.msgs[id]; ok {
			out[id] = tgclient.RemoteFile{MsgID: id, Caption: m.caption, Size: int64(len(m.data))}
		}
	}
	return out, nil
}

func (f *fakeTG) ScanChannel(ctx context.Context, fn func(tgclient.RemoteFile) error) error {
	return nil
}
func (f *fakeTG) ScanSince(ctx context.Context, min int, fn func(tgclient.RemoteFile) error) error {
	return nil
}

func (f *fakeTG) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.msgs)
}

type rig struct {
	t    *testing.T
	hub  *Hub
	srv  *httptest.Server
	tg   *fakeTG
	st   *store.Store
	dir  string
	keys map[string]string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	tg := &fakeTG{msgs: map[int]*fakeMsg{}}
	eng := backup.NewWith(st, tg, dir)
	eng.Configure(backup.Config{NoProjects: true})
	hub := New(st, eng, Options{DataDir: dir, PublicURL: "https://files.example"})
	ctx, cancel := context.WithCancel(context.Background())
	go eng.Run(ctx)
	go hub.Run(ctx)
	srv := httptest.NewServer(hub.Handler())
	t.Cleanup(func() { srv.Close(); cancel(); time.Sleep(700 * time.Millisecond); st.Close() })
	return &rig{t: t, hub: hub, srv: srv, tg: tg, st: st, dir: dir, keys: map[string]string{}}
}

func (r *rig) friend(name string, quota int64) string {
	key, hash := NewKey()
	if err := r.st.AddFriend(name, hash, quota); err != nil {
		r.t.Fatal(err)
	}
	r.keys[name] = key
	return key
}

func (r *rig) do(method, path, key string, body []byte) (*http.Response, []byte) {
	r.t.Helper()
	req, _ := http.NewRequest(method, r.srv.URL+path, bytes.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

func (r *rig) waitStored(name, rel string) {
	r.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		res, b := r.do("GET", "/v1/status/"+rel, r.keys[name], nil)
		if res.StatusCode == 200 && strings.Contains(string(b), `"stored"`) {
			return
		}
		time.Sleep(time.Second)
	}
	r.t.Fatalf("%s never reached Telegram", rel)
}

func TestKeysAndNames(t *testing.T) {
	k1, h1 := NewKey()
	k2, _ := NewKey()
	if k1 == k2 || HashKey(k1) != h1 || !strings.HasPrefix(k1, "tgv_") {
		t.Fatal("keys should be unique and hash consistently")
	}
	for name, want := range map[string]bool{"alice": true, "bob_2": true, "a": false, "Alice": false, "../x": false, "-bob": false} {
		if ValidName(name) != want {
			t.Errorf("ValidName(%q) != %v", name, want)
		}
	}
	for in, want := range map[string]int64{"50GB": 50 << 30, "1.5 tb": 3 << 39, "500mb": 500 << 20, "0": 0, "": 0} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := ParseSize("lots"); err == nil {
		t.Error("ParseSize should reject nonsense")
	}
}

func TestCleanRel(t *testing.T) {
	for p, ok := range map[string]bool{
		"a.txt": true, "dir/sub/a.txt": true, "/lead/slash.txt": true, "": true,
		"../x": false, "a/../../x": false, "a//b": false, "a/./b": false, `a\b`: false, "c:/x": false,
		"a/b ": false, "trail./x": false, "ctl\x01.txt": false,
	} {
		_, got := cleanRel(p)
		if got != ok {
			t.Errorf("cleanRel(%q) ok=%v, want %v", p, got, ok)
		}
	}
}

func TestAuthRequired(t *testing.T) {
	r := newRig(t)
	r.friend("alice", 0)
	for _, key := range []string{"", "tgv_wrong"} {
		if res, _ := r.do("GET", "/v1/me", key, nil); res.StatusCode != 401 {
			t.Errorf("key %q: status %d, want 401", key, res.StatusCode)
		}
	}
	if res, _ := r.do("GET", "/v1/me", r.keys["alice"], nil); res.StatusCode != 200 {
		t.Errorf("good key: status %d", res.StatusCode)
	}
	if err := r.st.SetFriendDisabled("alice", true); err != nil {
		t.Fatal(err)
	}
	if res, _ := r.do("GET", "/v1/me", r.keys["alice"], nil); res.StatusCode != 401 {
		t.Errorf("disabled friend: status %d, want 401", res.StatusCode)
	}
}

func TestWrongKeysGetBlocked(t *testing.T) {
	r := newRig(t)
	for i := 0; i < authTries; i++ {
		r.do("GET", "/v1/me", "tgv_wrong", nil)
	}
	r.friend("alice", 0)
	if res, _ := r.do("GET", "/v1/me", r.keys["alice"], nil); res.StatusCode != 429 {
		t.Errorf("after %d wrong keys even the right one is blocked for a while; got %d", authTries, res.StatusCode)
	}
}

func TestBadPathsAndUnstoredFiles(t *testing.T) {
	r := newRig(t)
	key := r.friend("alice", 0)
	for _, p := range []string{"/v1/files/a/%2e%2e/b.txt", "/v1/files/a%5Cb.txt"} {
		if res, _ := r.do("PUT", p, key, []byte("x")); res.StatusCode != 400 {
			t.Errorf("PUT %s: %d, want 400", p, res.StatusCode)
		}
	}
	if res, _ := r.do("PUT", "/v1/files/notes/~$draft.docx", key, []byte("x")); res.StatusCode != 422 {
		t.Errorf("temporary file: %d, want 422", res.StatusCode)
	}
	if res, _ := r.do("PUT", "/v1/files/proj/node_modules/x.js", key, []byte("x")); res.StatusCode != 422 {
		t.Errorf("node_modules file: %d, want 422", res.StatusCode)
	}
	if res, _ := r.do("PUT", "/v1/files/empty.txt", key, nil); res.StatusCode != 400 {
		t.Errorf("empty file: %d, want 400", res.StatusCode)
	}
	if res, _ := r.do("GET", "/v1/files/nothing.txt", key, nil); res.StatusCode != 404 {
		t.Errorf("missing file: %d, want 404", res.StatusCode)
	}
}

func TestQuota(t *testing.T) {
	r := newRig(t)
	key := r.friend("alice", 100)
	if res, _ := r.do("PUT", "/v1/files/big.bin", key, bytes.Repeat([]byte("x"), 101)); res.StatusCode != http.StatusInsufficientStorage {
		t.Errorf("101 bytes with a 100 byte limit: %d, want 507", res.StatusCode)
	}
	if res, _ := r.do("PUT", "/v1/files/ok.bin", key, bytes.Repeat([]byte("x"), 60)); res.StatusCode != http.StatusAccepted {
		t.Fatalf("60 bytes: %d, want 202", res.StatusCode)
	}
	if res, _ := r.do("PUT", "/v1/files/two.bin", key, bytes.Repeat([]byte("x"), 60)); res.StatusCode != http.StatusInsufficientStorage {
		t.Errorf("second 60 bytes goes over: %d, want 507", res.StatusCode)
	}
	// Replacing a file doesn't count the old copy twice.
	if res, _ := r.do("PUT", "/v1/files/ok.bin", key, bytes.Repeat([]byte("y"), 90)); res.StatusCode != http.StatusAccepted {
		t.Errorf("replacing within the limit: %d, want 202", res.StatusCode)
	}
}

// TestUploadStoreListDownloadShareDelete is the whole life of a file, against
// an in-memory Telegram. It waits for tgvault's quiet period, so it takes ~25s.
func TestUploadStoreListDownloadShareDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the upload quiet period")
	}
	r := newRig(t)
	alice, bob := r.friend("alice", 0), r.friend("bob", 0)
	content := bytes.Repeat([]byte("0123456789abcdef"), 200_000) // 3.2 MB, spans several preview blocks

	res, b := r.do("PUT", "/v1/files/photos/2025/trip.bin", alice, content)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("upload: %d %s", res.StatusCode, b)
	}
	r.waitStored("alice", "photos/2025/trip.bin")

	if r.tg.count() != 1 {
		t.Fatalf("Telegram holds %d messages, want 1", r.tg.count())
	}
	for _, m := range r.tg.msgs {
		if !strings.Contains(m.caption, `"p":"Friends/alice/photos/2025/trip.bin"`) {
			t.Errorf("caption has the wrong path: %s", m.caption)
		}
	}

	// The local copy is removed once it is safely in Telegram.
	spooled := filepath.Join(r.dir, "spool", "alice", "photos", "2025", "trip.bin")
	deadline := time.Now().Add(2 * releaseEvery)
	for _, err := os.Stat(spooled); err == nil && time.Now().Before(deadline); _, err = os.Stat(spooled) {
		time.Sleep(time.Second)
	}
	if _, err := os.Stat(spooled); err == nil {
		t.Error("the upload's local copy was never removed")
	}

	// Listing.
	_, b = r.do("GET", "/v1/list?path=photos", alice, nil)
	var l struct {
		Entries []entryOut
	}
	if err := json.Unmarshal(b, &l); err != nil || len(l.Entries) != 1 || l.Entries[0].Name != "2025" || l.Entries[0].Kind != "folder" {
		t.Fatalf("list: %s", b)
	}
	_, b = r.do("GET", "/v1/list?path=photos/2025", alice, nil)
	l.Entries = nil
	if err := json.Unmarshal(b, &l); err != nil || len(l.Entries) != 1 || l.Entries[0].Path != "photos/2025/trip.bin" || l.Entries[0].Size != int64(len(content)) {
		t.Fatalf("list: %s", b)
	}

	// Download, whole and by range.
	res, got := r.do("GET", "/v1/files/photos/2025/trip.bin", alice, nil)
	if res.StatusCode != 200 || !bytes.Equal(got, content) {
		t.Fatalf("download: %d, %d bytes (want %d)", res.StatusCode, len(got), len(content))
	}
	if cd := res.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	req, _ := http.NewRequest("GET", r.srv.URL+"/v1/files/photos/2025/trip.bin", nil)
	req.Header.Set("Authorization", "Bearer "+alice)
	req.Header.Set("Range", "bytes=1500000-1500015")
	rr, _ := http.DefaultClient.Do(req)
	part, _ := io.ReadAll(rr.Body)
	rr.Body.Close()
	if rr.StatusCode != 206 || !bytes.Equal(part, content[1500000:1500016]) {
		t.Errorf("range: %d %q", rr.StatusCode, part)
	}

	// Another friend can't see or fetch it.
	if res, _ := r.do("GET", "/v1/files/photos/2025/trip.bin", bob, nil); res.StatusCode != 404 {
		t.Errorf("bob fetching alice's file: %d, want 404", res.StatusCode)
	}
	_, b = r.do("GET", "/v1/list?path=photos", bob, nil)
	if !strings.Contains(string(b), `"entries":[]`) {
		t.Errorf("bob sees: %s", b)
	}

	// Share link: works without a key, only for that file.
	res, b = r.do("POST", "/v1/shares", alice, []byte(`{"path":"photos/2025/trip.bin","hours":1}`))
	var sh shareOut
	if res.StatusCode != 201 || json.Unmarshal(b, &sh) != nil || !strings.HasPrefix(sh.URL, "https://files.example/s/") {
		t.Fatalf("share: %d %s", res.StatusCode, b)
	}
	link := strings.TrimPrefix(sh.URL, "https://files.example")
	res, got = r.do("GET", link, "", nil)
	if res.StatusCode != 200 || !bytes.Equal(got, content) {
		t.Errorf("share download: %d, %d bytes", res.StatusCode, len(got))
	}
	if res, _ := r.do("GET", link+"x", "", nil); res.StatusCode != 404 {
		t.Errorf("wrong token: %d, want 404", res.StatusCode)
	}
	if res, _ := r.do("POST", "/v1/shares", bob, []byte(`{"path":"photos/2025/trip.bin"}`)); res.StatusCode != 404 {
		t.Errorf("bob sharing alice's file: %d, want 404", res.StatusCode)
	}
	if res, _ := r.do("DELETE", "/v1/shares/"+sh.ID, bob, nil); res.StatusCode != 404 {
		t.Errorf("bob revoking alice's link: %d, want 404", res.StatusCode)
	}
	if res, _ := r.do("DELETE", "/v1/shares/"+sh.ID, alice, nil); res.StatusCode != 200 {
		t.Errorf("revoke: %d", res.StatusCode)
	}
	if res, _ := r.do("GET", link, "", nil); res.StatusCode != 404 {
		t.Errorf("revoked link still works: %d", res.StatusCode)
	}

	// Delete removes it from Telegram too.
	if res, b := r.do("DELETE", "/v1/files/photos/2025/trip.bin", alice, nil); res.StatusCode != http.StatusAccepted {
		t.Fatalf("delete: %d %s", res.StatusCode, b)
	}
	deadline = time.Now().Add(10 * time.Second)
	for r.tg.count() > 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if r.tg.count() != 0 {
		t.Errorf("Telegram still holds %d messages after the delete", r.tg.count())
	}
	if res, _ := r.do("GET", "/v1/files/photos/2025/trip.bin", alice, nil); res.StatusCode != 404 {
		t.Errorf("deleted file: %d, want 404", res.StatusCode)
	}
}

func TestExpiredShareLink(t *testing.T) {
	r := newRig(t)
	r.friend("alice", 0)
	token := randomToken(24)
	if err := r.st.AddShare(store.Share{TokenHash: HashKey(token), Friend: "alice", CloudPath: "Friends/alice/x", Expires: time.Now().Add(-time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	if res, _ := r.do("GET", "/s/"+token, "", nil); res.StatusCode != 404 {
		t.Errorf("expired link: %d, want 404", res.StatusCode)
	}
}

func TestSortedFriends(t *testing.T) {
	r := newRig(t)
	r.friend("zed", 0)
	r.friend("amy", 0)
	list, _ := r.st.Friends()
	var names []string
	for _, f := range list {
		names = append(names, f.Name)
	}
	if !sort.StringsAreSorted(names) || len(names) != 2 {
		t.Errorf("friends = %v", names)
	}
	if err := r.st.AddFriend("amy", "other", 0); !errors.Is(err, store.ErrFriendExists) {
		t.Errorf("duplicate name: %v", err)
	}
}
