package proxy

import (
	"context"
	"encoding/base64"
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

const ClientID = "app_EMoamEEZ73f0CkXaXp7hrann" // Public OAuth client identifier, not a secret.
const AuthBase = "https://auth.openai.com"

type Credentials struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
}

// Auth owns a dedicated credential file. One process must own its refresh token.
type Auth struct {
	Path    string
	BaseURL string
	Client  *http.Client
	mu      sync.Mutex
	cached  *Credentials
	dirty   bool
}

func (a *Auth) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func (a *Auth) base() string {
	if a.BaseURL != "" {
		return strings.TrimRight(a.BaseURL, "/")
	}
	return AuthBase
}

func Claims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var v map[string]any
	_ = json.Unmarshal(b, &v)
	return v
}
func (c *Credentials) derive() {
	if c.ExpiresAt == 0 {
		if n, ok := Claims(c.AccessToken)["exp"].(float64); ok {
			c.ExpiresAt = int64(n)
		}
	}
	if c.AccountID == "" {
		for _, t := range []string{c.IDToken, c.AccessToken} {
			if v, ok := Claims(t)["https://api.openai.com/auth"].(map[string]any); ok {
				c.AccountID, _ = v["chatgpt_account_id"].(string)
				if c.AccountID != "" {
					break
				}
			}
		}
	}
}
func ReadCredentials(path string) (Credentials, error) {
	var c Credentials
	b, err := os.ReadFile(path)
	if err != nil {
		return c, errors.New("credentials unavailable; run login or import-auth")
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, errors.New("invalid credentials file")
	}
	// Accept LiteLLM's flat file and Codex's nested file, without retaining unrelated fields.
	var wrapper struct {
		Tokens *Credentials `json:"tokens"`
	}
	_ = json.Unmarshal(b, &wrapper)
	if wrapper.Tokens != nil {
		c = *wrapper.Tokens
	}
	c.derive()
	if c.AccessToken == "" || c.AccountID == "" {
		return c, errors.New("credentials need access_token and account_id")
	}
	return c, nil
}

func SaveCredentials(path string, c Credentials) error {
	c.derive()
	if c.AccessToken == "" || c.AccountID == "" {
		return errors.New("incomplete OAuth credentials")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("cannot create credential directory")
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".oauth-*")
	if err != nil {
		return errors.New("cannot create credential file")
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		return errors.New("cannot persist OAuth credentials")
	}
	d, err := os.Open(dir)
	if err == nil {
		err = d.Sync()
		_ = d.Close()
	}
	return err
}

func (a *Auth) Current(ctx context.Context) (Credentials, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cached == nil {
		c, err := ReadCredentials(a.Path)
		if err != nil {
			return Credentials{}, err
		}
		a.cached = &c
	}
	c := *a.cached
	if a.dirty {
		if err := SaveCredentials(a.Path, c); err != nil {
			return Credentials{}, err
		}
		a.dirty = false
	}
	if c.ExpiresAt > time.Now().Add(time.Minute).Unix() {
		return c, nil
	}
	if c.RefreshToken == "" {
		return Credentials{}, errors.New("credentials expired; login required")
	}
	refreshed, err := a.exchange(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.RefreshToken}, "client_id": {ClientID}})
	if err != nil {
		return Credentials{}, err
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = c.RefreshToken
	}
	if refreshed.IDToken == "" {
		refreshed.IDToken = c.IDToken
	}
	if refreshed.AccountID == "" {
		refreshed.AccountID = c.AccountID
	}
	refreshed.derive()
	// Retain a rotated token in memory even if disk persistence fails; never use it
	// for inference until persistence succeeds, and never retry the old refresh token.
	a.cached = &refreshed
	a.dirty = true
	if err = SaveCredentials(a.Path, refreshed); err != nil {
		return Credentials{}, err
	}
	a.dirty = false
	return refreshed, nil
}

func (a *Auth) exchange(ctx context.Context, values url.Values) (Credentials, error) {
	var c Credentials
	req, err := http.NewRequestWithContext(ctx, "POST", a.base()+"/oauth/token", strings.NewReader(values.Encode()))
	if err != nil {
		return c, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.client().Do(req)
	if err != nil {
		return c, errors.New("OAuth token endpoint unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return c, fmt.Errorf("OAuth token exchange failed (HTTP %d); login may be required", resp.StatusCode)
	}
	var result struct {
		Credentials
		ExpiresIn int64 `json:"expires_in"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return c, errors.New("invalid OAuth token response")
	}
	c = result.Credentials
	if c.AccessToken == "" {
		return c, errors.New("OAuth response missing access token")
	}
	c.derive()
	if c.ExpiresAt == 0 && result.ExpiresIn > 0 {
		c.ExpiresAt = time.Now().Unix() + result.ExpiresIn
	}
	if c.ExpiresAt == 0 {
		return c, errors.New("OAuth response missing token expiry")
	}
	return c, nil
}

func (a *Auth) Login(ctx context.Context, out io.Writer) error {
	var device struct {
		ID        string          `json:"device_auth_id"`
		Code      string          `json:"user_code"`
		Alternate string          `json:"usercode"`
		Interval  json.RawMessage `json:"interval"`
	}
	if err := a.postJSON(ctx, "/api/accounts/deviceauth/usercode", map[string]string{"client_id": ClientID}, &device); err != nil {
		return err
	}
	if device.Code == "" {
		device.Code = device.Alternate
	}
	if device.ID == "" || device.Code == "" {
		return errors.New("invalid device authorization response")
	}
	fmt.Fprintf(out, "Visit %s/codex/device and enter %s. Do not share this code.\n", a.base(), device.Code)
	interval := 5 * time.Second
	var raw any
	_ = json.Unmarshal(device.Interval, &raw)
	switch v := raw.(type) {
	case float64:
		if v > 5 && v < 60 {
			interval = time.Duration(v) * time.Second
		}
	case string:
		if d, e := time.ParseDuration(v + "s"); e == nil && d > interval && d < time.Minute {
			interval = d
		}
	}
	timer := time.NewTicker(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			var code struct {
				Code     string `json:"authorization_code"`
				Verifier string `json:"code_verifier"`
			}
			err := a.postJSON(ctx, "/api/accounts/deviceauth/token", map[string]string{"device_auth_id": device.ID, "user_code": device.Code}, &code)
			if errors.Is(err, errPending) {
				continue
			}
			if err != nil {
				return err
			}
			if code.Code == "" || code.Verifier == "" {
				return errors.New("invalid authorization code response")
			}
			c, err := a.exchange(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code.Code}, "code_verifier": {code.Verifier}, "redirect_uri": {a.base() + "/deviceauth/callback"}, "client_id": {ClientID}})
			if err != nil {
				return err
			}
			return SaveCredentials(a.Path, c)
		}
	}
}

var errPending = errors.New("authorization pending")

func (a *Auth) postJSON(ctx context.Context, path string, value any, dest any) error {
	b, _ := json.Marshal(value)
	r, err := http.NewRequestWithContext(ctx, "POST", a.base()+path, strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := a.client().Do(r)
	if err != nil {
		return errors.New("device authorization endpoint unavailable")
	}
	defer resp.Body.Close()
	if path == "/api/accounts/deviceauth/token" && (resp.StatusCode == 403 || resp.StatusCode == 404) {
		return errPending
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("device authorization failed (HTTP %d)", resp.StatusCode)
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(dest) != nil {
		return errors.New("invalid device authorization response")
	}
	return nil
}
