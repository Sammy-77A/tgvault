package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPlayKeys(t *testing.T) {
	var p playKeys
	k := p.add(7, "a/b.mkv")
	if got, ok := p.get(k); !ok || got.msg != 7 || got.entry != "a/b.mkv" {
		t.Fatalf("get = %+v, %v", got, ok)
	}
	// It works more than once: players ask for the file many times.
	if _, ok := p.get(k); !ok {
		t.Fatal("a key must keep working until it expires")
	}
	if _, ok := p.get("nope"); ok {
		t.Fatal("an unknown key must not work")
	}
	if k2 := p.add(7, ""); k2 == k || len(k2) != 32 {
		t.Fatalf("keys must be random and 128 bits long: %q %q", k, k2)
	}
	p.mu.Lock()
	e := p.m[k]
	e.expires = time.Now().Add(-time.Second)
	p.m[k] = e
	p.mu.Unlock()
	if _, ok := p.get(k); ok {
		t.Fatal("an expired key must not work")
	}
	if _, ok := p.m[k]; ok {
		t.Fatal("an expired key should be forgotten")
	}
}

func TestPlayLinkChecks(t *testing.T) {
	s := &Server{addr: "127.0.0.1:8790"}
	key := s.plays.add(1, "")
	get := func(remote, method, path string) int {
		r := httptest.NewRequest(method, path, nil)
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		s.play(w, r)
		return w.Code
	}
	if c := get("192.168.1.5:5555", "GET", "/play/"+key+"/x.mkv"); c != http.StatusForbidden {
		t.Fatalf("from another device: %d", c)
	}
	if c := get("127.0.0.1:5555", "POST", "/play/"+key+"/x.mkv"); c != http.StatusForbidden {
		t.Fatalf("a POST: %d", c)
	}
	if c := get("127.0.0.1:5555", "GET", "/play/wrongkey/x.mkv"); c != http.StatusNotFound {
		t.Fatalf("a wrong key: %d", c)
	}
	if c := get("[::1]:5555", "GET", "/play/wrongkey/x.mkv"); c != http.StatusNotFound {
		t.Fatalf("IPv6 loopback with a wrong key: %d", c)
	}
	if u := s.playURL(3, "", "Show S01E01 [720p].mkv"); !strings.HasPrefix(u, "http://127.0.0.1:8790/play/") || !strings.HasSuffix(u, "/Show%20S01E01%20%5B720p%5D.mkv") {
		t.Fatalf("play URL %q", u)
	}
}

// A play link needs no login cookie, but nothing else does.
func TestPlayLinksSkipTheCookieOnly(t *testing.T) {
	s := New(nil, nil, nil, nil, "tok", "127.0.0.1:8790", "test", func() {})
	r := httptest.NewRequest("GET", "/api/state", nil)
	r.Host = "127.0.0.1:8790"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("the API without the cookie: %d", w.Code)
	}
	r = httptest.NewRequest("GET", "/play/abc/x.mkv", nil)
	r.Host = "127.0.0.1:8790"
	r.RemoteAddr = "127.0.0.1:4000"
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("a play link with an unknown key: %d", w.Code)
	}
	// The host check still applies (DNS rebinding).
	r = httptest.NewRequest("GET", "/play/abc/x.mkv", nil)
	r.Host = "evil.example:8790"
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a play link on a foreign host: %d", w.Code)
	}
}
