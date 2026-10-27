package proxy

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

// newUpstreamClient is retained by one Server, so its routing cookies survive
// consecutive requests. Its default transport keeps the existing connection
// pooling and proxy configuration. Redirects remain disabled for OAuth requests.
func newUpstreamClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Minute,
		Jar:     newChatGPTCookieJar(),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// chatGPTCookieJar stores infrastructure routing cookies only; account, session,
// authentication and caller-supplied cookies never belong in this shared state.
// Scope, expiration and synchronization are handled by net/http/cookiejar.
// Source: OpenAI Codex 6aaac694803004a63c6f393d153c96d22521a6b9,
// codex-rs/http-client/src/chatgpt_cloudflare_cookies.rs (ChatGptCloudflareCookieStore).
// https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/http-client/src/chatgpt_cloudflare_cookies.rs
type chatGPTCookieJar struct {
	jar *cookiejar.Jar
}

func newChatGPTCookieJar() *chatGPTCookieJar {
	jar, _ := cookiejar.New(nil) // With nil options, New cannot return an error.
	return &chatGPTCookieJar{jar: jar}
}

func (j *chatGPTCookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	if !isChatGPTCookieURL(u) {
		return
	}
	allowed := make([]*http.Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		if cookie != nil && isInfrastructureCookie(cookie.Name) {
			allowed = append(allowed, cookie)
		}
	}
	j.jar.SetCookies(u, allowed)
}

func (j *chatGPTCookieJar) Cookies(u *url.URL) []*http.Cookie {
	if !isChatGPTCookieURL(u) {
		return nil
	}
	cookies := j.jar.Cookies(u)
	allowed := cookies[:0]
	for _, cookie := range cookies {
		if isInfrastructureCookie(cookie.Name) {
			allowed = append(allowed, cookie)
		}
	}
	return allowed
}

// Keep the exact allowlist aligned with OpenAI Codex's
// chatgpt_cloudflare_cookies.rs:is_allowed_cloudflare_cookie_name at the SHA above.
// __oailb is an infrastructure routing cookie, not an authentication cookie.
func isInfrastructureCookie(name string) bool {
	switch name {
	case "__cf_bm", "__cflb", "__cfruid", "__cfseq", "__cfwaitingroom", "__oailb",
		"_cfuvid", "cf_clearance", "cf_ob_info", "cf_use_ob":
		return true
	default:
		return strings.HasPrefix(name, "cf_chl_")
	}
}

// Source: OpenAI Codex at the same SHA, chatgpt_cloudflare_cookies.rs:
// is_chatgpt_cookie_url and codex-rs/http-client/src/chatgpt_hosts.rs.
func isChatGPTCookieURL(u *url.URL) bool {
	if u == nil || u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	switch host {
	case "chatgpt.com", "chat.openai.com", "chatgpt-staging.com":
		return true
	default:
		return strings.HasSuffix(host, ".chatgpt.com") || strings.HasSuffix(host, ".chatgpt-staging.com")
	}
}
