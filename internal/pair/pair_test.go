package pair

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func post(p *Listener, remote, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/pair", strings.NewReader(body))
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	p.handle(w, r)
	return w
}

func TestWrongCodesRotateTheCode(t *testing.T) {
	p := &Listener{code: "123456"}
	for i := 0; i < maxAttempts; i++ {
		if w := post(p, "192.168.1.5:5000", `{"code":"000000"}`); w.Code != http.StatusForbidden {
			t.Fatalf("attempt %d: status %d, want 403", i, w.Code)
		}
	}
	if p.code == "123456" {
		t.Fatal("the code must change after too many wrong guesses")
	}
	if len(p.code) != 6 {
		t.Fatalf("new code %q isn't 6 digits", p.code)
	}
}

func TestOnlyLocalNetwork(t *testing.T) {
	p := &Listener{code: "123456"}
	if w := post(p, "8.8.8.8:5000", `{"code":"123456"}`); w.Code != http.StatusForbidden {
		t.Fatalf("a public address must be refused, got %d", w.Code)
	}
	for _, ok := range []string{"192.168.137.1:1", "10.0.0.2:1", "172.16.0.9:1", "127.0.0.1:1"} {
		if !fromLocalNetwork(ok) {
			t.Errorf("%s should count as local", ok)
		}
	}
}

func TestSameAddrs(t *testing.T) {
	if !sameAddrs([]string{"192.168.1.2:4000"}, []string{"192.168.1.2"}) {
		t.Fatal("same IP, any port, is the same")
	}
	if sameAddrs([]string{"192.168.1.2:4000"}, []string{"192.168.1.3"}) {
		t.Fatal("a different IP must reopen the listener")
	}
}
