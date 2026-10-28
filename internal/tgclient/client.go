// Package tgclient connects to Telegram, handles login, and stores files in
// tgvault's private channel.
package tgclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/telegram/query/dialogs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"rsc.io/qr"

	"github.com/Sammy-77A/tgvault/internal/store"
)

// ChannelTitle is the name of the private channel files are stored in.
const ChannelTitle = "tgvault backup"

const channelAbout = "Files backed up by tgvault. Deleting messages here deletes those backups."

// Login states.
const (
	StatusNeedAPI      = "need_api"
	StatusConnecting   = "connecting"
	StatusNeedPhone    = "need_phone"
	StatusNeedCode     = "need_code"
	StatusNeedPassword = "need_password"
	StatusQR           = "need_qr"
	StatusReady        = "ready"
	StatusIdle         = "idle" // logged in, but disconnected until needed (phone)
)

// Setting keys.
const (
	keyAPIID       = "api_id"
	keyAPIHash     = "api_hash"
	keyChannelID   = "channel_id"
	keyChannelHash = "channel_hash"
	keyAccountID   = "account_id"
)

// Info describes the current connection for the UI.
type Info struct {
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
	Phone    string `json:"phone,omitempty"`
	CodeVia  string `json:"codeVia,omitempty"`  // where the login code was sent
	CodeNext string `json:"codeNext,omitempty"` // what "resend" would do
	QRURL    string `json:"qrUrl,omitempty"`    // set while QR login is waiting
	Account  string `json:"account,omitempty"`
	Channel  string `json:"channel,omitempty"`
	Online   bool   `json:"online"`
	FloodEnd int64  `json:"floodEnd,omitempty"`
}

// Client is tgvault's Telegram connection.
type Client struct {
	st      *store.Store
	dataDir string
	version string

	locs sync.Map // message ID -> fileLoc, for previews (see ReadBlock)

	mu        sync.Mutex
	status    string
	lastErr   string
	phone     string
	codeHash  string
	account   string
	online    bool
	floodEnd  time.Time
	sendMu    sync.Mutex // paces posts to the channel
	lastSend  time.Time
	sendGap   time.Duration
	sendOK    int
	codeVia   string
	codeNext  string
	emailCode bool
	qrURL     string
	qrCancel  context.CancelFunc
	runCtx    context.Context
	qrSignal  qrlogin.LoggedIn
	tc        *telegram.Client
	api       *tg.Client
	channel   *tg.InputChannel
	ready     chan struct{}
	authed    chan struct{}
	restart   chan struct{}
	onAccount func() // called when a different account logs in
	loginNote string // why this device was logged out, shown on the login screen
	device    telegram.DeviceConfig
	needed    func() bool // with idleAfter, whether to stay connected
	idleAfter time.Duration
}

// New creates a client. Call Run to connect.
func New(st *store.Store, dataDir, version string) *Client {
	return &Client{
		st:      st,
		dataDir: dataDir,
		version: version,
		status:  StatusNeedAPI,
		ready:   make(chan struct{}),
		authed:  make(chan struct{}, 1),
		restart: make(chan struct{}, 1),
		device:  telegram.DeviceConfig{DeviceModel: "PC", SystemVersion: "Windows"},
	}
}

// SetDevice sets how this device is named in Telegram's list of active sessions.
// Call before Run.
func (c *Client) SetDevice(model, system string) {
	c.device.DeviceModel, c.device.SystemVersion = model, system
}

// DisconnectWhenIdle makes the client drop its connection once needed has
// returned false for the grace period, and connect again as soon as it
// returns true. The phone uses it: Android freezes apps in the background,
// and a frozen app that still holds a connection can end up using its login
// from two connections at once, which makes Telegram end the login for good
// (AUTH_KEY_DUPLICATED). Call before Run.
func (c *Client) DisconnectWhenIdle(needed func() bool, grace time.Duration) {
	c.needed, c.idleAfter = needed, grace
}

// canIdle reports whether this device is logged in, so the connection can be
// dropped and picked up again without a new login.
func (c *Client) canIdle() bool {
	if c.needed == nil || c.st.Get(keyAccountID) == "" {
		return false
	}
	_, err := os.Stat(c.sessionPath())
	return err == nil
}

// waitNeeded parks the client until the connection is needed again. It
// reports false when ctx ends.
func (c *Client) waitNeeded(ctx context.Context) bool {
	c.setStatus(StatusIdle, "")
	log.Printf("telegram: disconnected while idle")
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for !c.needed() {
		select {
		case <-ctx.Done():
			return false
		case <-c.restart:
			return true
		case <-t.C:
		}
	}
	return true
}

// idleWatch cancels the connection once it hasn't been needed for the grace period.
func (c *Client) idleWatch(ctx context.Context, cancel context.CancelFunc) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	lastNeeded := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if c.needed() || c.Info().Status != StatusReady || !c.canIdle() {
			lastNeeded = time.Now()
		} else if time.Since(lastNeeded) >= c.idleAfter {
			cancel()
			return
		}
	}
}

// Reconnect closes every connection to Telegram and opens new ones. The phone
// calls it when it switches networks: Telegram ends a login whose key is used
// from two addresses at once, which happens if old connections linger on Wi-Fi
// while new ones go over mobile data.
func (c *Client) Reconnect() {
	c.mu.Lock()
	st := c.status
	c.mu.Unlock()
	// Don't interrupt a login in progress.
	if st == StatusReady || st == StatusConnecting || st == StatusIdle {
		c.kick()
	}
}

// OnAccountChange registers a callback for when a different account logs in.
func (c *Client) OnAccountChange(f func()) { c.onAccount = f }

func (c *Client) sessionPath() string { return filepath.Join(c.dataDir, "session.json") }

// Info returns the current state.
func (c *Client) Info() Info {
	c.mu.Lock()
	defer c.mu.Unlock()
	in := Info{Status: c.status, Error: c.lastErr, Phone: c.phone, Account: c.account, Online: c.online,
		CodeVia: c.codeVia, CodeNext: c.codeNext, QRURL: c.qrURL}
	if in.Error == "" && (in.Status == StatusNeedPhone || in.Status == StatusQR) {
		in.Error = c.loginNote
	}
	if c.status == StatusReady {
		in.Channel = ChannelTitle
	}
	if time.Now().Before(c.floodEnd) {
		in.FloodEnd = c.floodEnd.Unix()
	}
	return in
}

func (c *Client) setStatus(s, errMsg string) {
	c.mu.Lock()
	c.status, c.lastErr = s, errMsg
	c.mu.Unlock()
}

// Ready returns a channel that is closed once logged in with the channel ready.
func (c *Client) Ready() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready
}

// WaitReady blocks until ready or ctx ends.
func (c *Client) WaitReady(ctx context.Context) error {
	select {
	case <-c.Ready():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) conn() (*tg.Client, *tg.InputChannel, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status != StatusReady || c.api == nil || c.channel == nil {
		return nil, nil, errors.New("not connected to Telegram yet")
	}
	return c.api, c.channel, nil
}

// SetAPI stores the API credentials and (re)connects.
func (c *Client) SetAPI(id int, hash string) error {
	if id <= 0 || len(hash) != 32 {
		return errors.New("the API ID must be a number and the API hash 32 characters; copy both from my.telegram.org")
	}
	if err := c.st.Set(keyAPIID, strconv.Itoa(id)); err != nil {
		return err
	}
	if err := c.st.Set(keyAPIHash, hash); err != nil {
		return err
	}
	c.kick()
	return nil
}

func (c *Client) kick() {
	select {
	case c.restart <- struct{}{}:
	default:
	}
}

// Run keeps a Telegram connection alive until ctx ends.
func (c *Client) Run(ctx context.Context) {
	for ctx.Err() == nil {
		id, _ := strconv.Atoi(c.st.Get(keyAPIID))
		hash := c.st.Get(keyAPIHash)
		if id == 0 || hash == "" {
			c.setStatus(StatusNeedAPI, "")
			select {
			case <-ctx.Done():
				return
			case <-c.restart:
				continue
			}
		}

		if c.canIdle() && !c.needed() {
			if !c.waitNeeded(ctx) {
				return
			}
		}

		runCtx, cancel := context.WithCancel(ctx)
		if c.needed != nil {
			go c.idleWatch(runCtx, cancel)
		}
		go func() {
			select {
			case <-c.restart:
				cancel()
			case <-runCtx.Done():
			}
		}()
		err := c.runOnce(runCtx, id, hash)
		restarted := runCtx.Err() != nil && ctx.Err() == nil
		cancel()
		c.disconnect()
		if ctx.Err() != nil {
			return
		}
		if restarted {
			continue
		}
		if deadSession(err) {
			// Retrying can't help: the key is gone, so start a fresh login.
			log.Printf("telegram: login ended (%v); removing the session", err)
			if rmErr := os.Remove(c.sessionPath()); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				log.Printf("telegram: %v", rmErr)
			}
			c.mu.Lock()
			c.loginNote = friendly(err)
			c.mu.Unlock()
			continue
		}
		msg := "lost connection to Telegram; reconnecting"
		if err != nil {
			msg = friendly(err)
			log.Printf("telegram: %v", err)
		}
		c.setStatus(StatusConnecting, msg)
		select {
		case <-ctx.Done():
			return
		case <-c.restart:
		case <-time.After(15 * time.Second):
		}
	}
}

func (c *Client) disconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.online = false
	c.api = nil
	c.tc = nil
	c.channel = nil
	select {
	case <-c.ready:
		c.ready = make(chan struct{})
	default:
	}
}

func (c *Client) runOnce(ctx context.Context, id int, hash string) error {
	c.setStatus(StatusConnecting, "")
	// Updates are only needed so QR login learns when the code was scanned.
	dispatcher := tg.NewUpdateDispatcher()
	qrSignal := qrlogin.OnLoginToken(dispatcher)
	tc := telegram.NewClient(id, hash, telegram.Options{
		UpdateHandler:  dispatcher,
		SessionStorage: &session.FileStorage{Path: c.sessionPath()},
		Device: telegram.DeviceConfig{
			DeviceModel:    c.device.DeviceModel,
			SystemVersion:  c.device.SystemVersion,
			AppVersion:     "tgvault " + c.version,
			SystemLangCode: "en",
			LangCode:       "en",
		},
		Middlewares: []telegram.Middleware{telegram.MiddlewareFunc(c.floodWaiter), telegram.MiddlewareFunc(c.retryStale)},
		OnConnectionState: func(s telegram.ConnectionState) {
			c.mu.Lock()
			c.online = s == telegram.ConnectionStateReady
			c.mu.Unlock()
		},
	})
	return tc.Run(ctx, func(ctx context.Context) error {
		c.mu.Lock()
		c.tc, c.api, c.online = tc, tc.API(), true
		c.runCtx, c.qrSignal = ctx, qrSignal
		c.mu.Unlock()

		st, err := tc.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("check login: %w", err)
		}
		if !st.Authorized {
			c.setStatus(StatusNeedPhone, "")
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-c.authed:
			}
		}
		self, err := tc.Self(ctx)
		if err != nil {
			return fmt.Errorf("read account: %w", err)
		}
		if err := c.checkAccount(self.ID); err != nil {
			return err
		}
		ch, err := c.ensureChannel(ctx, tc.API())
		if err != nil {
			return fmt.Errorf("prepare the backup channel: %w", err)
		}
		name := self.FirstName
		if self.LastName != "" {
			name += " " + self.LastName
		}
		if self.Username != "" {
			name += " (@" + self.Username + ")"
		}
		c.mu.Lock()
		c.channel = ch
		c.account = name
		c.status, c.lastErr = StatusReady, ""
		close(c.ready)
		c.mu.Unlock()
		log.Printf("telegram: ready as %s", name)
		<-ctx.Done()
		return ctx.Err()
	})
}

// checkAccount clears channel and cloud data when a different account logs in.
func (c *Client) checkAccount(userID int64) error {
	prev := c.st.Get(keyAccountID)
	cur := strconv.FormatInt(userID, 10)
	if prev == cur {
		return nil
	}
	if prev != "" {
		log.Printf("telegram: different account logged in; forgetting the previous account's file list")
		if err := c.st.Delete(keyChannelID, keyChannelHash); err != nil {
			return err
		}
		if err := c.st.ClearCloud(); err != nil {
			return err
		}
		if c.onAccount != nil {
			c.onAccount()
		}
	}
	return c.st.Set(keyAccountID, cur)
}

// floodWaiter sleeps through short "slow down" replies from Telegram and
// retries; longer ones are returned so the caller can reschedule.
func (c *Client) floodWaiter(next tg.Invoker) telegram.InvokeFunc {
	return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		for attempt := 0; ; attempt++ {
			err := next.Invoke(ctx, input, output)
			d, ok := tgerr.AsFloodWait(err)
			if !ok {
				return err
			}
			c.mu.Lock()
			if end := time.Now().Add(d); end.After(c.floodEnd) {
				c.floodEnd = end
			}
			c.mu.Unlock()
			if d > 60*time.Second || attempt >= 3 {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d + time.Second):
			}
		}
	}
}

// staleRetries and staleDelay control retryStale: how often a request is sent
// again after the connection turned out to be dead, and the wait before that.
var (
	staleRetries = 3
	staleDelay   = 700 * time.Millisecond
)

// staleConn reports whether err is the connection itself failing (a socket
// the network or Android closed behind our back), not a reply from Telegram.
func staleConn(err error) bool {
	if err == nil || tgerr.Is(err) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	s := err.Error()
	for _, frag := range []string{"broken pipe", "connection reset", "connection aborted", "use of closed network connection",
		"forcibly closed", "wsasend", "wsarecv"} {
		if strings.Contains(s, frag) {
			return true
		}
	}
	return false
}

// retryStale sends a request again when the connection under it had silently
// died ("write: broken pipe"). gotd opens a new connection by itself, but the
// request that found the old one dead is returned as an error; retrying it a
// moment later, once the new connection is up, makes that invisible. The same
// request is sent, so Telegram sees the same random ID and never stores a
// message twice.
func (c *Client) retryStale(next tg.Invoker) telegram.InvokeFunc {
	return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		for attempt := 0; ; attempt++ {
			err := next.Invoke(ctx, input, output)
			if attempt >= staleRetries || ctx.Err() != nil || !staleConn(err) {
				return err
			}
			log.Printf("telegram: the connection had gone stale (%v); retrying (%d of %d)", err, attempt+1, staleRetries)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(staleDelay * time.Duration(attempt+1)):
			}
			c.waitOnline(ctx, 10*time.Second)
		}
	}
}

// waitOnline waits until gotd reports a working connection, for at most max.
func (c *Client) waitOnline(ctx context.Context, max time.Duration) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		on := c.online
		c.mu.Unlock()
		if on {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// ---- login ----

func (c *Client) loginClient() (*telegram.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tc == nil {
		return nil, errors.New("still connecting to Telegram; try again in a moment")
	}
	return c.tc, nil
}

// SubmitPhone asks Telegram to send a login code.
func (c *Client) SubmitPhone(ctx context.Context, phone string) error {
	tc, err := c.loginClient()
	if err != nil {
		return err
	}
	phone = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(phone)
	sent, err := tc.Auth().SendCode(ctx, phone, auth.SendCodeOptions{})
	if err != nil {
		return errors.New(friendly(err))
	}
	return c.codeSent(phone, sent)
}

// ResendCode asks Telegram to send the code again, usually another way (SMS or call).
func (c *Client) ResendCode(ctx context.Context) error {
	tc, err := c.loginClient()
	if err != nil {
		return err
	}
	c.mu.Lock()
	phone, hash := c.phone, c.codeHash
	c.mu.Unlock()
	if phone == "" {
		return errors.New("enter your phone number first")
	}
	sent, err := tc.Auth().ResendCode(ctx, phone, hash)
	if err != nil {
		if tgerr.Is(err, "SEND_CODE_UNAVAILABLE") {
			return errors.New("Telegram has no other way to send the code; use QR login instead")
		}
		return errors.New(friendly(err))
	}
	return c.codeSent(phone, sent)
}

func (c *Client) codeSent(phone string, sent tg.AuthSentCodeClass) error {
	code, ok := sent.(*tg.AuthSentCode)
	if !ok {
		return errors.New("Telegram did not send a login code; try QR login instead")
	}
	if _, ok := code.Type.(*tg.AuthSentCodeTypeSetUpEmailRequired); ok {
		return errors.New(`Telegram wants this account to set up a login email before new apps can get codes; use "Log in with a QR code" instead`)
	}
	via, email := describeCodeType(code.Type)
	next := ""
	if nt, ok := code.GetNextType(); ok {
		next = describeNextType(nt)
	}
	c.mu.Lock()
	c.phone, c.codeHash = phone, code.PhoneCodeHash
	c.codeVia, c.codeNext, c.emailCode = via, next, email
	c.status, c.lastErr = StatusNeedCode, ""
	c.mu.Unlock()
	log.Printf("telegram: login code sent %s", via)
	return nil
}

func describeCodeType(t tg.AuthSentCodeTypeClass) (via string, email bool) {
	switch v := t.(type) {
	case *tg.AuthSentCodeTypeApp:
		return `as a message from "Telegram" in your Telegram app on your other devices (open that chat; there may be no notification)`, false
	case *tg.AuthSentCodeTypeSMS, *tg.AuthSentCodeTypeSMSWord, *tg.AuthSentCodeTypeSMSPhrase, *tg.AuthSentCodeTypeFirebaseSMS:
		return "by SMS", false
	case *tg.AuthSentCodeTypeCall:
		return "by a phone call that reads out the code", false
	case *tg.AuthSentCodeTypeFlashCall, *tg.AuthSentCodeTypeMissedCall:
		return "as a missed call (the code is the last digits of the calling number)", false
	case *tg.AuthSentCodeTypeEmailCode:
		return "to your login email " + v.EmailPattern, true
	case *tg.AuthSentCodeTypeFragmentSMS:
		return "through Fragment (" + v.URL + ")", false
	}
	return "to your Telegram account", false
}

func describeNextType(t tg.AuthCodeTypeClass) string {
	switch t.(type) {
	case *tg.AuthCodeTypeSMS:
		return "Send it by SMS instead"
	case *tg.AuthCodeTypeCall:
		return "Call me with the code instead"
	case *tg.AuthCodeTypeFlashCall, *tg.AuthCodeTypeMissedCall:
		return "Send it as a missed call instead"
	}
	return "Send the code again"
}

// SubmitCode signs in with the login code.
func (c *Client) SubmitCode(ctx context.Context, code string) error {
	tc, err := c.loginClient()
	if err != nil {
		return err
	}
	c.mu.Lock()
	phone, hash, email := c.phone, c.codeHash, c.emailCode
	c.mu.Unlock()
	if email {
		// Codes sent by email go in a different field than phone codes.
		_, err = tc.API().AuthSignIn(ctx, &tg.AuthSignInRequest{
			PhoneNumber:       phone,
			PhoneCodeHash:     hash,
			EmailVerification: &tg.EmailVerificationCode{Code: code},
		})
		if tgerr.Is(err, "SESSION_PASSWORD_NEEDED") {
			err = auth.ErrPasswordAuthNeeded
		}
	} else {
		_, err = tc.Auth().SignIn(ctx, phone, code, hash)
	}
	if errors.Is(err, auth.ErrPasswordAuthNeeded) {
		c.setStatus(StatusNeedPassword, "")
		return nil
	}
	var signUp *auth.SignUpRequired
	if errors.As(err, &signUp) {
		return errors.New("this phone number has no Telegram account; sign up in the Telegram app first")
	}
	if err != nil {
		return errors.New(friendly(err))
	}
	c.loggedIn()
	return nil
}

// StartQR begins QR-code login: the user scans the code with the Telegram app
// on their phone (Settings > Devices > Link Desktop Device).
func (c *Client) StartQR() error {
	tc, err := c.loginClient()
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.qrCancel != nil {
		c.mu.Unlock()
		return nil // already waiting
	}
	ctx, cancel := context.WithTimeout(c.runCtx, 10*time.Minute)
	c.qrCancel = cancel
	c.status, c.lastErr, c.qrURL = StatusQR, "", ""
	signal := c.qrSignal
	c.mu.Unlock()

	go func() {
		defer cancel()
		_, err := tc.QR().Auth(ctx, signal, func(_ context.Context, t qrlogin.Token) error {
			c.mu.Lock()
			c.qrURL = t.URL()
			c.mu.Unlock()
			return nil
		})
		c.mu.Lock()
		c.qrCancel, c.qrURL = nil, ""
		c.mu.Unlock()
		switch {
		case err == nil:
			c.loggedIn()
		case tgerr.Is(err, "SESSION_PASSWORD_NEEDED") || errors.Is(err, auth.ErrPasswordAuthNeeded):
			c.setStatus(StatusNeedPassword, "")
		case errors.Is(err, context.Canceled):
			// Cancelled by the user or a reconnect; status is set elsewhere.
		case errors.Is(err, context.DeadlineExceeded):
			c.setStatus(StatusNeedPhone, "the QR code wasn't scanned in time; try again")
		default:
			log.Printf("telegram: QR login: %v", err)
			c.setStatus(StatusNeedPhone, "QR login failed: "+friendly(err))
		}
	}()
	return nil
}

// CancelQR stops waiting for a scan and goes back to phone login.
func (c *Client) CancelQR() {
	c.mu.Lock()
	cancel := c.qrCancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.setStatus(StatusNeedPhone, "")
}

// QRImage returns the current login QR code as a PNG.
func (c *Client) QRImage() ([]byte, error) {
	c.mu.Lock()
	u := c.qrURL
	c.mu.Unlock()
	if u == "" {
		return nil, errors.New("no QR code yet")
	}
	code, err := qr.Encode(u, qr.M)
	if err != nil {
		return nil, err
	}
	code.Scale = 8 // large enough for a phone camera
	return code.PNG(), nil
}

// SubmitPassword completes login for accounts with two-step verification.
func (c *Client) SubmitPassword(ctx context.Context, password string) error {
	tc, err := c.loginClient()
	if err != nil {
		return err
	}
	if _, err := tc.Auth().Password(ctx, password); err != nil {
		if errors.Is(err, auth.ErrPasswordInvalid) {
			return errors.New("wrong password")
		}
		return errors.New(friendly(err))
	}
	c.loggedIn()
	return nil
}

func (c *Client) loggedIn() {
	c.mu.Lock()
	c.loginNote = ""
	c.mu.Unlock()
	c.setStatus(StatusConnecting, "")
	select {
	case c.authed <- struct{}{}:
	default:
	}
}

// Logout logs out of Telegram and deletes the local session.
func (c *Client) Logout(ctx context.Context) error {
	c.mu.Lock()
	tc := c.tc
	c.mu.Unlock()
	if tc != nil {
		if _, err := tc.API().AuthLogOut(ctx); err != nil {
			log.Printf("telegram: logout: %v", err)
		}
	}
	if err := os.Remove(c.sessionPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	c.mu.Lock()
	c.phone, c.codeHash, c.account = "", "", ""
	c.codeVia, c.codeNext, c.emailCode = "", "", false
	c.mu.Unlock()
	c.kick()
	return nil
}

// ---- channel ----

func (c *Client) ensureChannel(ctx context.Context, api *tg.Client) (*tg.InputChannel, error) {
	if id, _ := strconv.ParseInt(c.st.Get(keyChannelID), 10, 64); id != 0 {
		hash, _ := strconv.ParseInt(c.st.Get(keyChannelHash), 10, 64)
		ch := &tg.InputChannel{ChannelID: id, AccessHash: hash}
		res, err := api.ChannelsGetChannels(ctx, []tg.InputChannelClass{ch})
		if err == nil && channelUsable(res, id) {
			return ch, nil
		}
		log.Printf("telegram: saved channel is not usable (%v); looking for it again", err)
	}

	// Look for an existing tgvault channel, e.g. after reinstalling.
	var found *tg.InputChannel
	errStop := errors.New("stop")
	err := query.GetDialogs(api).BatchSize(100).ForEach(ctx, func(ctx context.Context, e dialogs.Elem) error {
		for _, ch := range e.Entities.Channels() {
			if ch.Title == ChannelTitle && ch.Creator && !ch.Left && ch.Broadcast {
				found = ch.AsInput()
				return errStop
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return nil, err
	}
	if found == nil {
		upd, err := api.ChannelsCreateChannel(ctx, &tg.ChannelsCreateChannelRequest{
			Broadcast: true,
			Title:     ChannelTitle,
			About:     channelAbout,
		})
		if err != nil {
			return nil, err
		}
		found = channelFromUpdates(upd)
		if found == nil {
			return nil, errors.New("Telegram did not return the new channel")
		}
		log.Printf("telegram: created channel %q", ChannelTitle)
	}
	if err := c.st.Set(keyChannelID, strconv.FormatInt(found.ChannelID, 10)); err != nil {
		return nil, err
	}
	if err := c.st.Set(keyChannelHash, strconv.FormatInt(found.AccessHash, 10)); err != nil {
		return nil, err
	}
	return found, nil
}

func channelUsable(res tg.MessagesChatsClass, id int64) bool {
	for _, chat := range res.GetChats() {
		if ch, ok := chat.(*tg.Channel); ok && ch.ID == id && !ch.Left {
			return true
		}
	}
	return false
}

func channelFromUpdates(u tg.UpdatesClass) *tg.InputChannel {
	var chats []tg.ChatClass
	switch v := u.(type) {
	case *tg.Updates:
		chats = v.Chats
	case *tg.UpdatesCombined:
		chats = v.Chats
	}
	for _, chat := range chats {
		if ch, ok := chat.(*tg.Channel); ok {
			return ch.AsInput()
		}
	}
	return nil
}

// friendly turns Telegram errors into plain sentences.
func friendly(err error) string {
	switch {
	case err == nil:
		return ""
	case tgerr.Is(err, "API_ID_INVALID", "API_ID_PUBLISHED_FLOOD"):
		return "Telegram rejected the API ID/hash; check them at my.telegram.org"
	case tgerr.Is(err, "PHONE_NUMBER_INVALID"):
		return "Telegram says that phone number is invalid; include the country code, e.g. +254..."
	case tgerr.Is(err, "PHONE_CODE_INVALID"):
		return "wrong code; check the latest code Telegram sent"
	case tgerr.Is(err, "PHONE_CODE_EXPIRED"):
		return "that code expired; go back and request a new one"
	case tgerr.Is(err, "PHONE_NUMBER_BANNED"):
		return "Telegram says this phone number is banned"
	case tgerr.Is(err, "AUTH_KEY_DUPLICATED"):
		return "Telegram ended this device's login because it saw it connected from two networks at once (for example Wi-Fi and mobile data); log in again"
	case tgerr.Is(err, "AUTH_KEY_UNREGISTERED", "SESSION_REVOKED", "SESSION_EXPIRED", "USER_DEACTIVATED"):
		return "this login was ended from another device; log in again"
	}
	if d, ok := tgerr.AsFloodWait(err); ok {
		return fmt.Sprintf("Telegram asked to wait %s before trying again", d.Round(time.Second))
	}
	return err.Error()
}

// deadSession reports whether Telegram has ended this login for good, so the
// saved session is useless.
func deadSession(err error) bool {
	return err != nil && tgerr.Is(err, "AUTH_KEY_DUPLICATED", "AUTH_KEY_UNREGISTERED", "SESSION_REVOKED", "SESSION_EXPIRED")
}

// Friendly is exported for other packages.
func Friendly(err error) string { return friendly(err) }

// APICredentials returns the stored API ID and hash (zero values if not set).
func (c *Client) APICredentials() (int, string) {
	id, _ := strconv.Atoi(c.st.Get(keyAPIID))
	return id, c.st.Get(keyAPIHash)
}

// LoginLink starts QR login if needed and returns the current tg://login link,
// so another tgvault that is logged in can approve it. It waits for the
// connection and the first token until ctx ends.
func (c *Client) LoginLink(ctx context.Context) (string, error) {
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for {
		info := c.Info()
		switch info.Status {
		case StatusReady:
			return "", errors.New("this device is already logged in")
		case StatusNeedPassword:
			return "", errors.New("this device is waiting for your two-step verification password; enter it there")
		case StatusQR:
			if info.QRURL != "" {
				return info.QRURL, nil
			}
		case StatusNeedPhone, StatusNeedCode:
			if err := c.StartQR(); err != nil {
				return "", err
			}
		}
		select {
		case <-ctx.Done():
			return "", errors.New("the device didn't get a login code from Telegram in time; check its internet connection")
		case <-t.C:
		}
	}
}

// AcceptLogin approves a tg://login link from another device, logging that
// device in to this account. It is what scanning a QR code in the Telegram app does.
func (c *Client) AcceptLogin(ctx context.Context, link string) error {
	tok, err := qrlogin.ParseTokenURL(link)
	if err != nil {
		return fmt.Errorf("not a Telegram login link: %w", err)
	}
	api, _, err := c.conn()
	if err != nil {
		return err
	}
	if _, err := qrlogin.AcceptQR(ctx, api, tok); err != nil {
		return errors.New(friendly(err))
	}
	return nil
}
