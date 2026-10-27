package proxy

import (
	"net/http"
	"strings"
)

// Keep this list explicit: caller credentials, account selectors, cookies and
// transport headers must never enter the OAuth request. These compatibility
// headers come from native Codex's build_responses_headers, compatibility_headers
// and build_session_headers, and CLIProxyAPI's applyCodexHeadersWithSIWC:
// https://github.com/openai/codex/tree/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs
// https://github.com/router-for-me/CLIProxyAPI/blob/1f917e77c4562623aa49e9804d999524151a2f96/internal/runtime/executor/codex_executor_request.go#L437-L447
var codexRequestHeaders = []string{
	"Session-Id", "Thread-Id", "Session_id", "Conversation_id",
	"X-Codex-Turn-State", "X-Codex-Turn-Metadata", "X-Codex-Window-Id",
	"X-Codex-Parent-Thread-Id", "X-OpenAI-Subagent", "X-Codex-Beta-Features",
	"X-OpenAI-Internal-Codex-Responses-Lite", "X-Client-Request-Id", "Version",
	"Originator", "User-Agent",
}

func setUpstreamHeaders(dst, src http.Header, body map[string]any, c Credentials) {
	for _, name := range codexRequestHeaders {
		copyHeader(dst, src, name)
	}
	// LiteLLM extracts these identity spellings in this order, and its subscription
	// provider emits session_id. Normalize only when modern session-id is absent.
	// A supplied stable cache key is the final fallback; never invent an identity
	// per request or change the independently controlled body cache key.
	// https://github.com/BerriAI/litellm/blob/4fa8bf1ff5515e2cb98908c62ba84c87ef938620/litellm/proxy/litellm_pre_call_utils.py#L95
	// https://github.com/BerriAI/litellm/blob/4fa8bf1ff5515e2cb98908c62ba84c87ef938620/litellm/proxy/litellm_pre_call_utils.py#L742-L767
	// https://github.com/BerriAI/litellm/blob/4fa8bf1ff5515e2cb98908c62ba84c87ef938620/litellm/llms/chatgpt/common_utils.py#L228-L245
	// https://github.com/router-for-me/CLIProxyAPI/blob/1f917e77c4562623aa49e9804d999524151a2f96/internal/runtime/executor/codex_executor_request.go#L129-L185
	if strings.TrimSpace(dst.Get("Session-Id")) == "" {
		identity := ""
		for _, name := range []string{"Session_id", "Thread-Id", "Conversation_id"} {
			if candidate := dst.Get(name); strings.TrimSpace(candidate) != "" {
				identity = candidate
				break
			}
		}
		if identity == "" {
			identity, _ = body["prompt_cache_key"].(string)
		}
		if strings.TrimSpace(identity) != "" && validHeaderValue(identity) {
			dst.Set("Session-Id", identity)
		}
	}
	if strings.TrimSpace(dst.Get("Originator")) == "" {
		dst.Set("Originator", "chatgpt-oauth")
	}
	if strings.TrimSpace(dst.Get("User-Agent")) == "" {
		dst.Set("User-Agent", "chatgpt-oauth/0.1")
	}
	dst.Set("Authorization", "Bearer "+c.AccessToken)
	dst.Set("ChatGPT-Account-ID", c.AccountID)
	dst.Set("Content-Type", "application/json")
	dst.Set("Accept", "text/event-stream")
	// Derive this only after request conversion/normalization. Native clients may
	// name a gateway alias, while the backend needs the final upstream model/tier.
	// https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/core/src/client.rs#L1280-L1295
	if model, ok := body["model"].(string); ok && model != "" {
		hint := "model=" + model
		if tier, ok := body["service_tier"].(string); ok {
			hint += ";tier=" + tier
		}
		if validHeaderValue(hint) {
			dst.Set("X-Codex-Routing-Hint", hint)
		}
	}
}

// Native Codex consumes this response metadata before processing SSE. Preserve
// turn state even on errors so continuation/retry handling sees upstream state.
// https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/codex-api/src/sse/responses.rs#L32-L95
// https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/codex-api/src/safety_buffering.rs#L4-L19
var codexResponseHeaders = []string{
	"Retry-After", "Retry-After-Ms", "X-Request-ID", "OpenAI-Model",
	"X-Codex-Turn-State", "X-Reasoning-Included",
	"X-Codex-Safety-Buffering-Enabled", "X-Codex-Safety-Buffering-Faster-Model",
	"X-Codex-Credits-Has-Credits", "X-Codex-Credits-Unlimited", "X-Codex-Credits-Balance",
	"X-Codex-Promo-Message", "X-Codex-Rate-Limit-Reached-Type",
}

func copyUpstreamHeaders(dst, src http.Header) {
	for _, name := range codexResponseHeaders {
		copyHeader(dst, src, name)
	}
	// Metered limits have dynamic names. Native Codex discovers each family from
	// its primary-used-percent header; copy only the recognized members of those
	// families, alongside the default legacy Codex family.
	// https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/codex-api/src/rate_limits.rs#L29-L101
	prefixes := map[string]bool{"x-codex": true}
	for name := range src {
		name = strings.ToLower(name)
		if prefix, ok := strings.CutSuffix(name, "-primary-used-percent"); ok && strings.HasPrefix(prefix, "x-") {
			prefixes[prefix] = true
		}
	}
	for prefix := range prefixes {
		for _, suffix := range []string{
			"-primary-used-percent", "-primary-window-minutes", "-primary-reset-at", "-primary-reset-after-seconds",
			"-secondary-used-percent", "-secondary-window-minutes", "-secondary-reset-at", "-secondary-reset-after-seconds",
			"-limit-name",
		} {
			copyHeader(dst, src, prefix+suffix)
		}
	}
	// X-Models-Etag describes the upstream account catalog. This adapter serves
	// an independently configured model list and its gateway serves a filtered
	// catalog, so propagating that unrelated validator would invalidate the wrong
	// catalog. The layer that owns discovery must supply its own validator.
}

func copyHeader(dst, src http.Header, name string) {
	values := src.Values(name)
	if len(values) == 0 {
		return
	}
	for _, value := range values {
		if !validHeaderValue(value) {
			return
		}
	}
	dst[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
}

func validHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if (value[i] < 0x20 && value[i] != '\t') || value[i] == 0x7f {
			return false
		}
	}
	return true
}
