// Package pair lets a tgvault that is already logged in (the PC) log in a new
// device (the phone) over the local network. A phone can't scan its own login
// QR code, and Telegram's login codes don't always arrive, so instead the
// phone shows an address and a 6-digit code, and the PC approves the login.
//
// The phone listens only while it isn't logged in. The PC sends its API ID
// and hash, the phone answers with its tg://login link, and the PC accepts it.
package pair

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

const maxAttempts = 5 // wrong codes before a new code is chosen

// Request is what the PC sends to the phone.
type Request struct {
	Code    string `json:"code"`
	APIID   int    `json:"apiId"`
	APIHash string `json:"apiHash"`
}

// Response is the phone's answer.
type Response struct {
	Link  string `json:"link,omitempty"`
	Error string `json:"error,omitempty"`
}

// Info is shown on the phone so the user can type it on the PC.
type Info struct {
	Addresses []string `json:"addresses"`
	Code      string   `json:"code"`
}

// Listener accepts pairing requests while the device isn't logged in.
type Listener struct {
	tg *tgclient.Client

	mu       sync.Mutex
	l        net.Listener
	srv      *http.Server
	code     string
	attempts int
	addrs    []string
}

// NewListener creates a listener for tg. Call Run to start it.
func NewListener(tg *tgclient.Client) *Listener { return &Listener{tg: tg} }

// Run opens the listener whenever the device is logged out, until ctx ends.
func (p *Listener) Run(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	defer p.close()
	for {
		if st := p.tg.Info().Status; st == tgclient.StatusReady || st == tgclient.StatusIdle {
			p.close()
		} else if err := p.open(); err != nil {
			log.Printf("pair: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Info returns the address and code to show, or nil when not listening.
func (p *Listener) Info() *Info {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.l == nil {
		return nil
	}
	return &Info{Addresses: p.addrs, Code: p.code}
}

func (p *Listener) open() error {
	ips := localIPs()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.l != nil && sameAddrs(p.addrs, ips) {
		return nil
	}
	if p.l != nil { // the network changed: listen again so the shown address is right
		p.srv.Close()
		p.l, p.srv = nil, nil
	}
	if len(ips) == 0 {
		return nil // no network yet; try again later
	}
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		return err
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	p.addrs = p.addrs[:0]
	for _, ip := range ips {
		p.addrs = append(p.addrs, net.JoinHostPort(ip, port))
	}
	p.l, p.code, p.attempts = l, newCode(), 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /pair", p.handle)
	p.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go p.srv.Serve(l)
	log.Printf("pair: waiting for the PC at %s", strings.Join(p.addrs, ", "))
	return nil
}

func (p *Listener) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.l == nil {
		return
	}
	p.srv.Close()
	p.l, p.srv = nil, nil
}

func (p *Listener) handle(w http.ResponseWriter, r *http.Request) {
	reply := func(status int, resp Response) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(resp)
	}
	if !fromLocalNetwork(r.RemoteAddr) {
		reply(http.StatusForbidden, Response{Error: "only devices on the same network can pair"})
		return
	}
	var req Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		reply(http.StatusBadRequest, Response{Error: "bad request"})
		return
	}
	p.mu.Lock()
	ok := subtle.ConstantTimeCompare([]byte(req.Code), []byte(p.code)) == 1
	if !ok {
		p.attempts++
		if p.attempts >= maxAttempts {
			p.code, p.attempts = newCode(), 0 // stop guessing; the phone shows the new code
		}
	}
	p.mu.Unlock()
	if !ok {
		reply(http.StatusForbidden, Response{Error: "wrong code; check the code shown on the phone"})
		return
	}
	if id, hash := p.tg.APICredentials(); id != req.APIID || hash != req.APIHash {
		if err := p.tg.SetAPI(req.APIID, req.APIHash); err != nil {
			reply(http.StatusBadRequest, Response{Error: err.Error()})
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	link, err := p.tg.LoginLink(ctx)
	if err != nil {
		reply(http.StatusConflict, Response{Error: err.Error()})
		return
	}
	reply(http.StatusOK, Response{Link: link})
}

// Approve is the PC side: it sends this device's API credentials to the phone
// at addr, receives the phone's login link and accepts it.
func Approve(ctx context.Context, tg *tgclient.Client, addr, code string) error {
	addr = strings.TrimSpace(addr)
	code = strings.TrimSpace(code)
	host, _, err := net.SplitHostPort(addr)
	if err != nil || net.ParseIP(host) == nil {
		return errors.New("enter the address exactly as the phone shows it, like 192.168.1.23:40123")
	}
	if !isLocalIP(net.ParseIP(host)) {
		return errors.New("that address isn't on your local network")
	}
	id, hash := tg.APICredentials()
	if id == 0 {
		return errors.New("log in to Telegram on this PC first")
	}
	body, _ := json.Marshal(Request{Code: code, APIID: id, APIHash: hash})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/pair", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach the phone at %s; check both are on the same Wi-Fi and tgvault is open on the phone", addr)
	}
	defer res.Body.Close()
	var resp Response
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		return errors.New("the phone gave an unexpected answer; is it running tgvault?")
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	return tg.AcceptLogin(ctx, resp.Link)
}

func sameAddrs(addrs, ips []string) bool {
	if len(addrs) != len(ips) {
		return false
	}
	for i, a := range addrs {
		if host, _, _ := net.SplitHostPort(a); host != ips[i] {
			return false
		}
	}
	return true
}

func newCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%06d", n.Int64())
}

// localIPs returns this device's private IPv4 addresses.
func localIPs() []string {
	var out []string
	seen := map[string]bool{}
	add := func(ip net.IP) {
		if ip4 := ip.To4(); ip4 != nil && isLocalIP(ip4) && !seen[ip4.String()] {
			seen[ip4.String()] = true
			out = append(out, ip4.String())
		}
	}
	// Android blocks listing network interfaces for apps, so first ask which
	// address would be used to reach the internet (no packets are sent).
	if c, err := net.Dial("udp4", "192.0.2.1:9"); err == nil {
		add(c.LocalAddr().(*net.UDPAddr).IP)
		c.Close()
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				add(n.IP)
			}
		}
	}
	return out
}

func isLocalIP(ip net.IP) bool { return ip.IsPrivate() || ip.IsLinkLocalUnicast() }

func fromLocalNetwork(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && (isLocalIP(ip) || ip.IsLoopback())
}
