package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Delegated scopes: read-only access to files and sites the signed-in user can already open.
const scopes = "offline_access openid profile User.Read Files.Read.All Sites.Read.All"

// ErrNeedLogin means there is no usable token and the user must complete a device-code sign-in.
var ErrNeedLogin = errors.New("sign-in required")

type token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// DeviceCode is what the user needs to finish signing in.
type DeviceCode struct {
	UserCode        string    `json:"userCode"`
	VerificationURI string    `json:"verificationUri"`
	Message         string    `json:"message"`
	ExpiresAt       time.Time `json:"expiresAt"`
}

// Auth implements the OAuth 2.0 device authorization grant against Microsoft Entra ID
// with a refresh-token cache on disk. No third-party dependencies.
type Auth struct {
	clientID  string
	authority string // e.g. https://login.microsoftonline.com/organizations
	cachePath string
	hc        *http.Client
	log       io.Writer

	mu      sync.Mutex
	tok     *token
	pending *DeviceCode
	lastErr string
	running bool
}

// NewAuth creates an authenticator. loginBase is the Entra ID host, normally
// https://login.microsoftonline.com (sovereign clouds use a different one).
func NewAuth(clientID, tenant, loginBase, cachePath string, log io.Writer) *Auth {
	a := &Auth{
		clientID:  clientID,
		authority: strings.TrimRight(loginBase, "/") + "/" + url.PathEscape(tenant),
		cachePath: cachePath,
		hc:        &http.Client{Timeout: 30 * time.Second},
		log:       log,
	}
	a.loadCache()
	return a
}

func defaultCachePath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "sptree", "token.json")
}

func (a *Auth) loadCache() {
	b, err := os.ReadFile(a.cachePath)
	if err != nil {
		return
	}
	var t token
	if json.Unmarshal(b, &t) == nil && t.RefreshToken != "" {
		a.tok = &t
	}
}

func (a *Auth) saveCache() {
	if a.tok == nil {
		_ = os.Remove(a.cachePath)
		return
	}
	if err := os.MkdirAll(filepath.Dir(a.cachePath), 0o700); err != nil {
		fmt.Fprintln(a.log, "warning: cannot create token cache dir:", err)
		return
	}
	b, _ := json.Marshal(a.tok)
	if err := os.WriteFile(a.cachePath, b, 0o600); err != nil {
		fmt.Fprintln(a.log, "warning: cannot write token cache:", err)
	}
}

// SignedIn reports whether a refresh token is available.
func (a *Auth) SignedIn() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tok != nil
}

// Status returns the current device-code prompt (if a sign-in is in progress) and the last error.
func (a *Auth) Status() (pending *DeviceCode, lastErr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pending != nil {
		p := *a.pending
		pending = &p
	}
	return pending, a.lastErr
}

// Logout forgets the cached token.
func (a *Auth) Logout() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tok = nil
	a.saveCache()
}

// AccessToken returns a valid access token, refreshing it when needed.
// force=true refreshes even if the cached token looks valid (used after a 401).
func (a *Auth) AccessToken(ctx context.Context, force bool) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tok == nil {
		return "", ErrNeedLogin
	}
	if !force && a.tok.AccessToken != "" && time.Until(a.tok.ExpiresAt) > 2*time.Minute {
		return a.tok.AccessToken, nil
	}
	form := url.Values{
		"client_id":     {a.clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {a.tok.RefreshToken},
		"scope":         {scopes},
	}
	t, oerr, err := a.tokenRequest(ctx, form)
	if err != nil {
		return "", err
	}
	if oerr != nil {
		// invalid_grant etc.: the refresh token is no longer usable.
		a.tok = nil
		a.saveCache()
		a.lastErr = oerr.Error()
		return "", ErrNeedLogin
	}
	a.tok = t
	a.saveCache()
	return t.AccessToken, nil
}

type oauthError struct {
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

func (e *oauthError) Error() string {
	d := e.Description
	if i := strings.Index(d, "\r\n"); i > 0 {
		d = d[:i]
	}
	return e.Code + ": " + d
}

func (a *Auth) tokenRequest(ctx context.Context, form url.Values) (*token, *oauthError, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", a.authority+"/oauth2/v2.0/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		var oe oauthError
		if json.Unmarshal(body, &oe) == nil && oe.Code != "" {
			return nil, &oe, nil
		}
		return nil, nil, fmt.Errorf("token endpoint: HTTP %d", resp.StatusCode)
	}
	var r struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, nil, err
	}
	t := &token{AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, ExpiresAt: time.Now().Add(time.Duration(r.ExpiresIn) * time.Second)}
	if t.RefreshToken == "" && a.tok != nil {
		t.RefreshToken = a.tok.RefreshToken
	}
	return t, nil, nil
}

// StartDeviceLogin begins a device-code sign-in in the background. It is a no-op if one is running.
func (a *Auth) StartDeviceLogin() {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return
	}
	a.running = true
	a.lastErr = ""
	a.mu.Unlock()

	go func() {
		err := a.deviceLogin(context.Background())
		a.mu.Lock()
		a.running = false
		a.pending = nil
		if err != nil {
			a.lastErr = err.Error()
			fmt.Fprintln(a.log, "sign-in failed:", err)
		}
		a.mu.Unlock()
	}()
}

func (a *Auth) deviceLogin(ctx context.Context) error {
	form := url.Values{"client_id": {a.clientID}, "scope": {scopes}}
	req, _ := http.NewRequestWithContext(ctx, "POST", a.authority+"/oauth2/v2.0/devicecode", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.hc.Do(req)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != 200 {
		var oe oauthError
		if json.Unmarshal(body, &oe) == nil && oe.Code != "" {
			return &oe
		}
		return fmt.Errorf("devicecode endpoint: HTTP %d", resp.StatusCode)
	}
	var dc struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
		Message         string `json:"message"`
	}
	if err := json.Unmarshal(body, &dc); err != nil {
		return err
	}
	interval := time.Duration(max(dc.Interval, 5)) * time.Second
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)

	a.mu.Lock()
	a.pending = &DeviceCode{UserCode: dc.UserCode, VerificationURI: dc.VerificationURI, Message: dc.Message, ExpiresAt: deadline}
	a.mu.Unlock()
	fmt.Fprintf(a.log, "\nSign in to Microsoft 365: open %s and enter the code %s\n\n", dc.VerificationURI, dc.UserCode)

	poll := url.Values{
		"client_id":   {a.clientID},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {dc.DeviceCode},
	}
	for time.Now().Before(deadline) {
		time.Sleep(interval)
		t, oerr, err := a.tokenRequest(ctx, poll)
		if err != nil {
			continue // transient network error: keep polling until the code expires
		}
		if oerr != nil {
			switch oerr.Code {
			case "authorization_pending":
				continue
			case "slow_down":
				interval += 5 * time.Second
				continue
			default: // authorization_declined, expired_token, bad_verification_code, ...
				return oerr
			}
		}
		a.mu.Lock()
		a.tok = t
		a.saveCache()
		a.mu.Unlock()
		fmt.Fprintln(a.log, "Signed in.")
		return nil
	}
	return errors.New("the sign-in code expired; start again")
}
