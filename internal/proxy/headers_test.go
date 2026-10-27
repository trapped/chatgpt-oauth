package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestConversationHeaderRoundTrip(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "streaming"}[stream], func(t *testing.T) {
			var calls int
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				want := map[string]string{
					"Session-Id": "stable-session", "Thread-Id": "thread-1",
					"X-Codex-Turn-Metadata": `{"turn_id":"turn-1"}`, "X-Codex-Window-Id": "window-1",
					"X-Codex-Parent-Thread-Id": "parent-1", "X-OpenAI-Subagent": "collab_spawn",
					"X-Codex-Beta-Features": "feature-a,feature-b", "X-OpenAI-Internal-Codex-Responses-Lite": "true",
					"X-Client-Request-Id": "client-request-1", "Version": "0.164.0",
					"Originator": "codex_cli_rs", "User-Agent": "codex_cli_rs/0.164.0 (test)",
					"X-Codex-Routing-Hint": "model=test-model;tier=priority",
					"ChatGPT-Account-ID":   "test-account", "Content-Type": "application/json", "Accept": "text/event-stream",
				}
				if calls == 2 {
					want["X-Codex-Turn-State"] = "upstream-turn-token"
				}
				for name, value := range want {
					if got := r.Header.Get(name); got != value {
						t.Errorf("request %d header %s = %q, want %q", calls, name, got, value)
					}
				}
				if r.Header.Get("Authorization") == "Bearer backend-only" || r.Header.Get("Cookie") != "" || r.Header.Get("OpenAI-Organization") != "" || r.Header.Get("OpenAI-Project") != "" || r.Header.Get("X-Private-Header") != "" {
					t.Error("untrusted credentials/account/cookies/headers reached OAuth upstream")
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["prompt_cache_key"] != "independent-body-cache-key" || body["service_tier"] != "priority" {
					t.Error("header derivation changed body settings")
				}
				w.Header().Set("X-Codex-Turn-State", "upstream-turn-token")
				w.Header().Set("OpenAI-Model", "upstream-resolved-model")
				w.Header()["X-Reasoning-Included"] = []string{""} // Presence alone has meaning to Codex.
				w.Header().Set("Set-Cookie", "__oailb=private-routing-cookie")
				w.Header().Set("X-Models-Etag", "upstream-account-catalog")
				w.Header().Set("X-Codex-Primary-Window-Minutes", "300")
				w.Header().Set("X-Codex-Primary-Reset-At", "1234")
				w.Header().Set("X-Codex-Other-Primary-Used-Percent", "25")
				w.Header().Set("X-Codex-Other-Limit-Name", "Other models")
				w.Header().Set("X-Codex-Credits-Has-Credits", "true")
				w.Header().Set("X-Codex-Safety-Buffering-Enabled", "true")
				sse(w, terminal())
			}))
			defer upstream.Close()
			s := &Server{Auth: testAuth(t), BaseURL: upstream.URL, Models: []string{"test-model"}, Token: "backend-only"}
			turnState := ""
			for range 2 {
				body := `{"model":"test-model","input":"stable input","prompt_cache_key":"independent-body-cache-key","service_tier":"priority","stream":` + map[bool]string{false: "false", true: "true"}[stream] + `}`
				r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
				for name, value := range map[string]string{
					"Authorization": "Bearer backend-only", "ChatGPT-Account-ID": "caller-account",
					"OpenAI-Organization": "caller-org", "OpenAI-Project": "caller-project", "Cookie": "caller=secret",
					"X-Private-Header": "secret", "X-Codex-Routing-Hint": "model=stale-alias;tier=default",
					"Session-Id": "stable-session", "Thread-Id": "thread-1",
					"X-Codex-Turn-Metadata": `{"turn_id":"turn-1"}`, "X-Codex-Window-Id": "window-1",
					"X-Codex-Parent-Thread-Id": "parent-1", "X-OpenAI-Subagent": "collab_spawn",
					"X-Codex-Beta-Features": "feature-a,feature-b", "X-OpenAI-Internal-Codex-Responses-Lite": "true",
					"X-Client-Request-Id": "client-request-1", "Version": "0.164.0",
					"Originator": "codex_cli_rs", "User-Agent": "codex_cli_rs/0.164.0 (test)",
				} {
					r.Header.Set(name, value)
				}
				if turnState != "" {
					r.Header.Set("X-Codex-Turn-State", turnState)
				}
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				if w.Code != http.StatusOK {
					t.Fatalf("status %d: %s", w.Code, w.Body.String())
				}
				turnState = w.Header().Get("X-Codex-Turn-State")
				if turnState != "upstream-turn-token" || w.Header().Get("OpenAI-Model") != "upstream-resolved-model" {
					t.Fatal("lost upstream continuity/model metadata")
				}
				if _, present := w.Header()["X-Reasoning-Included"]; !present {
					t.Error("lost empty reasoning marker")
				}
				for name, value := range map[string]string{
					"X-Codex-Primary-Window-Minutes": "300", "X-Codex-Primary-Reset-At": "1234",
					"X-Codex-Other-Primary-Used-Percent": "25", "X-Codex-Other-Limit-Name": "Other models",
					"X-Codex-Credits-Has-Credits": "true", "X-Codex-Safety-Buffering-Enabled": "true",
				} {
					if w.Header().Get(name) != value {
						t.Errorf("lost response %s", name)
					}
				}
				if w.Header().Get("Set-Cookie") != "" || w.Header().Get("X-Models-Etag") != "" {
					t.Error("leaked routing cookie or unrelated catalog validator")
				}
			}
		})
	}
}

func TestSessionIdentityFallbackAndRoutingHint(t *testing.T) {
	for _, tc := range []struct {
		name, session, legacy, thread, conversation, cacheKey, tier, wantSession, wantHint string
	}{
		{"modern takes precedence", "modern", "", "independent-thread", "", "cache", "priority", "modern", "model=test-model;tier=priority"},
		{"legacy supported", "", "legacy", "independent-thread", "", "cache", "", "legacy", "model=test-model"},
		{"thread fallback", "", "", "thread", "conversation", "cache", "", "thread", "model=test-model"},
		{"conversation fallback", "", "", "", "conversation", "cache", "", "conversation", "model=test-model"},
		{"stable cache fallback", "", "", "", "", "cache", "default", "cache", "model=test-model;tier=default"},
		{"no invented identity", "", "", "", "", "", "", "", "model=test-model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := http.Header{}
			if tc.session != "" {
				src.Set("Session-Id", tc.session)
			}
			if tc.legacy != "" {
				src.Set("Session_id", tc.legacy)
			}
			if tc.thread != "" {
				src.Set("Thread-Id", tc.thread)
			}
			if tc.conversation != "" {
				src.Set("Conversation_id", tc.conversation)
			}
			src.Set("X-Codex-Routing-Hint", "model=stale-alias")
			body := map[string]any{"model": "test-model", "prompt_cache_key": tc.cacheKey}
			if tc.tier != "" {
				body["service_tier"] = tc.tier
			}
			before, _ := json.Marshal(body)
			for range 2 {
				dst := http.Header{}
				setUpstreamHeaders(dst, src, body, Credentials{AccessToken: "oauth-secret", AccountID: "oauth-account"})
				if dst.Get("Session-Id") != tc.wantSession || dst.Get("Thread-Id") != tc.thread || dst.Get("Session_id") != tc.legacy || dst.Get("Conversation_id") != tc.conversation {
					t.Fatalf("incorrect identity: %v", dst)
				}
				if dst.Get("X-Codex-Routing-Hint") != tc.wantHint {
					t.Errorf("routing hint %q", dst.Get("X-Codex-Routing-Hint"))
				}
				if dst.Get("Originator") != "chatgpt-oauth" || dst.Get("User-Agent") != "chatgpt-oauth/0.1" {
					t.Error("defaults must identify the actual adapter")
				}
			}
			after, _ := json.Marshal(body)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("headers mutated request body")
			}
		})
	}
}

func TestInvalidHeadersNeverReachUpstream(t *testing.T) {
	for _, value := range []string{"line\r\ninjection", "null\x00value", "del\x7fvalue"} {
		src := http.Header{}
		src.Set("Session-Id", value)
		src.Set("X-Codex-Turn-State", value)
		src.Set("User-Agent", value)
		dst := http.Header{}
		setUpstreamHeaders(dst, src, map[string]any{"model": "test-model", "prompt_cache_key": "safe-fallback"}, Credentials{})
		if dst.Get("Session-Id") != "safe-fallback" || dst.Get("X-Codex-Turn-State") != "" || dst.Get("User-Agent") != "chatgpt-oauth/0.1" {
			t.Fatalf("unsafe headers were not removed: %v", dst)
		}
	}
}

func TestUpstreamErrorPreservesRetryAndTurnHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.Header().Set("X-Request-ID", "upstream-request")
		w.Header().Set("X-Codex-Turn-State", "retry-state")
		w.Header().Set("X-Codex-Rate-Limit-Reached-Type", "usage_limit")
		w.Header().Set("Authorization", "Bearer upstream-secret")
		w.Header().Set("Set-Cookie", "secret=cookie")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, "private upstream response")
	}))
	defer upstream.Close()
	s := &Server{Auth: testAuth(t), BaseURL: upstream.URL, Models: []string{"test-model"}}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","input":"hi"}`)))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "3" || w.Header().Get("X-Request-ID") != "upstream-request" || w.Header().Get("X-Codex-Turn-State") != "retry-state" || w.Header().Get("X-Codex-Rate-Limit-Reached-Type") != "usage_limit" {
		t.Fatalf("lost upstream retry metadata: status=%d headers=%v", w.Code, w.Header())
	}
	if w.Header().Get("Authorization") != "" || w.Header().Get("Set-Cookie") != "" || strings.Contains(w.Body.String(), "private") {
		t.Fatal("upstream secrets escaped boundary")
	}
}
