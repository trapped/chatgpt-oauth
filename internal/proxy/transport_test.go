package proxy

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func cookieURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func cookieNames(cookies []*http.Cookie) string {
	names := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		names = append(names, cookie.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func TestChatGPTCookieJarInfrastructureAllowlist(t *testing.T) {
	jar := newChatGPTCookieJar()
	u := cookieURL(t, "https://chatgpt.com/backend-api/codex/responses")
	names := []string{"__cf_bm", "__cflb", "__cfruid", "__cfseq", "__cfwaitingroom", "__oailb", "_cfuvid", "cf_clearance", "cf_ob_info", "cf_use_ob", "cf_chl_rc_i"}
	var cookies []*http.Cookie
	for _, name := range names {
		cookies = append(cookies, &http.Cookie{Name: name, Value: "infra", Path: "/", Secure: true})
	}
	for _, name := range []string{"__Secure-next-auth.session-token", "chatgpt_session", "oai-auth-token", "not_cf_clearance", "__CFLB", "caller"} {
		cookies = append(cookies, &http.Cookie{Name: name, Value: "private", Path: "/", Secure: true})
	}
	jar.SetCookies(u, cookies)
	sort.Strings(names)
	if got, want := cookieNames(jar.Cookies(u)), strings.Join(names, ","); got != want {
		t.Fatalf("stored names %q, want %q", got, want)
	}
}

func TestChatGPTCookieJarTrustedHTTPSHosts(t *testing.T) {
	for _, raw := range []string{
		"https://chatgpt.com/a", "https://foo.chatgpt.com/a", "https://chat.openai.com/a",
		"https://chatgpt-staging.com/a", "https://api.chatgpt-staging.com/a",
	} {
		t.Run(raw, func(t *testing.T) {
			jar := newChatGPTCookieJar()
			u := cookieURL(t, raw)
			jar.SetCookies(u, []*http.Cookie{{Name: "__oailb", Value: "route", Path: "/"}})
			if got := cookieNames(jar.Cookies(u)); got != "__oailb" {
				t.Fatalf("allowed host did not retain cookie: %q", got)
			}
		})
	}
	for _, raw := range []string{
		"http://chatgpt.com/a", "wss://chatgpt.com/a", "https://api.openai.com/a",
		"https://evilchatgpt.com/a", "https://chatgpt.com.evil.example/a", "https://foo.chat.openai.com/a",
	} {
		t.Run(raw, func(t *testing.T) {
			jar := newChatGPTCookieJar()
			u := cookieURL(t, raw)
			jar.SetCookies(u, []*http.Cookie{{Name: "__oailb", Value: "route", Path: "/"}})
			if got := jar.Cookies(u); len(got) != 0 {
				t.Fatalf("untrusted URL retained cookies: %v", got)
			}
			if got := jar.Cookies(cookieURL(t, "https://chatgpt.com/a")); len(got) != 0 {
				t.Fatalf("untrusted URL populated trusted jar: %v", got)
			}
		})
	}
	if isChatGPTCookieURL(nil) {
		t.Fatal("nil URL was accepted")
	}
}

func TestChatGPTCookieJarScopeAndExpiry(t *testing.T) {
	jar := newChatGPTCookieJar()
	u := cookieURL(t, "https://chatgpt.com/backend-api/codex/responses")
	jar.SetCookies(u, []*http.Cookie{{Name: "__oailb", Value: "route", Path: "/backend-api", Secure: true, MaxAge: 3600}})
	followup := cookieURL(t, "https://chatgpt.com/backend-api/ps/mcp")
	if got := jar.Cookies(followup); len(got) != 1 || got[0].Value != "route" {
		t.Fatalf("missing in-scope cookie: %v", got)
	}
	for _, raw := range []string{
		"https://chatgpt.com/", "https://chatgpt.com/backend-apix", "https://other.chatgpt.com/backend-api/ps/mcp",
		"https://api.openai.com/backend-api/ps/mcp", "http://chatgpt.com/backend-api/ps/mcp",
	} {
		if got := jar.Cookies(cookieURL(t, raw)); len(got) != 0 {
			t.Errorf("cookie escaped scope to %s: %v", raw, got)
		}
	}
	jar.SetCookies(u, []*http.Cookie{{Name: "__oailb", Path: "/backend-api", MaxAge: -1}})
	if got := jar.Cookies(followup); len(got) != 0 {
		t.Fatalf("expired cookie remained: %v", got)
	}
	jar.SetCookies(u, []*http.Cookie{{Name: "__cflb", Value: "expired", Path: "/", Expires: time.Now().Add(-time.Hour)}})
	if got := jar.Cookies(followup); len(got) != 0 {
		t.Fatalf("past Expires cookie remained: %v", got)
	}
}

func TestChatGPTCookieJarDomainScope(t *testing.T) {
	jar := newChatGPTCookieJar()
	u := cookieURL(t, "https://api.chatgpt.com/a")
	jar.SetCookies(u, []*http.Cookie{{Name: "__cflb", Value: "shared", Domain: ".chatgpt.com", Path: "/", Secure: true}})
	if got := jar.Cookies(cookieURL(t, "https://chatgpt.com/b")); len(got) != 1 || got[0].Value != "shared" {
		t.Fatalf("valid domain cookie was lost: %v", got)
	}
	if got := jar.Cookies(cookieURL(t, "https://chatgpt-staging.com/b")); len(got) != 0 {
		t.Fatalf("domain cookie crossed hosts: %v", got)
	}
	jar.SetCookies(u, []*http.Cookie{{Name: "__oailb", Value: "invalid", Domain: "chatgpt-staging.com", Path: "/"}})
	if got := jar.Cookies(cookieURL(t, "https://chatgpt-staging.com/b")); len(got) != 0 {
		t.Fatalf("foreign Domain attribute was accepted: %v", got)
	}
}

func TestChatGPTCookieJarConcurrentAccess(t *testing.T) {
	jar := newChatGPTCookieJar()
	u := cookieURL(t, "https://chatgpt.com/backend-api/codex/responses")
	var workers sync.WaitGroup
	for worker := range 24 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for iteration := range 50 {
				jar.SetCookies(u, []*http.Cookie{{Name: "__oailb", Value: fmt.Sprintf("%d-%d", worker, iteration), Path: "/", Secure: true}})
				if got := jar.Cookies(u); len(got) != 1 || got[0].Name != "__oailb" {
					t.Errorf("concurrent cookie access lost routing cookie: %v", got)
					return
				}
			}
		}()
	}
	workers.Wait()
}

func TestNewUpstreamClientHasIndependentCookieState(t *testing.T) {
	first, second := newUpstreamClient(), newUpstreamClient()
	if first.Timeout != 10*time.Minute {
		t.Fatalf("timeout = %v", first.Timeout)
	}
	u := cookieURL(t, "https://chatgpt.com/backend-api/codex/responses")
	first.Jar.SetCookies(u, []*http.Cookie{{Name: "__oailb", Value: "first", Path: "/", Secure: true}})
	if len(first.Jar.Cookies(u)) != 1 || len(second.Jar.Cookies(u)) != 0 {
		t.Fatal("separate upstream clients share cookie state")
	}
}

func TestNewUpstreamClientDoesNotFollowRedirects(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	resp, err := newUpstreamClient().Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || redirected.Load() != 0 {
		t.Fatalf("redirect followed: status %d, target requests %d", resp.StatusCode, redirected.Load())
	}
}

func TestNewUpstreamClientReusesConnection(t *testing.T) {
	var connections atomic.Int32
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	origin.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	origin.Start()
	defer origin.Close()
	client := newUpstreamClient()
	for range 3 {
		resp, err := client.Get(origin.URL)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("opened %d connections for consecutive requests, want 1", got)
	}
}
