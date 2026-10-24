// Package tgclient connects to Telegram, handles login, and stores files in
// tgvault's private channel.
package tgclient

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

	mu        sync.Mutex
	status    string
	lastErr   string
	phone     string
	codeHash  string
	account   string
	online    bool
	floodEnd  time.Time
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

		runCtx, cancel := context.WithCancel(ctx)
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
			DeviceModel:    "PC",
			SystemVersion:  "Windows",
			AppVersion:     "tgvault " + c.version,
			SystemLangCode: "en",
			LangCode:       "en",
		},
		Middlewares: []telegram.Middleware{telegram.MiddlewareFunc(c.floodWaiter)},
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
	case tgerr.Is(err, "AUTH_KEY_UNREGISTERED", "SESSION_REVOKED", "USER_DEACTIVATED"):
		return "this login was ended from another device; log in again"
	}
	if d, ok := tgerr.AsFloodWait(err); ok {
		return fmt.Sprintf("Telegram asked to wait %s before trying again", d.Round(time.Second))
	}
	return err.Error()
}

// Friendly is exported for other packages.
func Friendly(err error) string { return friendly(err) }
