# Codex HTTP protocol compatibility

This adapter implements the stateless Responses HTTP/SSE path. The table maps
its compatibility behavior to immutable reference implementations. Sources
describe protocol behavior; they do not guarantee upstream cache hits.

| Behavior | Source | Verification |
| --- | --- | --- |
| Preserve native session/thread identity and turn state, including the response turn-state header. The client controls when a turn starts and ends. | [Codex session affinity](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/core/src/client.rs#L657), [request headers](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/codex-api/src/requests/headers.rs), [response headers](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/codex-api/src/sse/responses.rs) | Header and mock-server round-trip tests. |
| Recognize legacy session aliases without replacing an explicit native identity. Use a nonempty body cache key only when no client identity is supplied. | [LiteLLM alias extraction](https://github.com/BerriAI/litellm/blob/4fa8bf1ff5515e2cb98908c62ba84c87ef938620/litellm/proxy/litellm_pre_call_utils.py#L742), [legacy upstream session header](https://github.com/BerriAI/litellm/blob/4fa8bf1ff5515e2cb98908c62ba84c87ef938620/litellm/llms/chatgpt/common_utils.py#L228), [CLIProxyAPI cache-key fallback](https://github.com/router-for-me/CLIProxyAPI/blob/1f917e77c4562623aa49e9804d999524151a2f96/internal/runtime/executor/codex_executor_request.go#L129) | Explicit identity, alias precedence, fallback and empty-value tests. |
| Preserve native client/turn metadata and the Responses Lite marker when supplied. Derive the routing hint from the final upstream model and service tier. | [Codex routing hint](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/core/src/client.rs#L1280), [turn metadata](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/core/src/responses_metadata.rs#L382), [CLIProxyAPI header policy](https://github.com/router-for-me/CLIProxyAPI/blob/1f917e77c4562623aa49e9804d999524151a2f96/internal/runtime/executor/codex_executor_request.go#L437) | Header allowlist and resolved routing-hint tests. |
| Retain only native infrastructure cookies in one reusable upstream client. Restrict storage and retrieval to trusted ChatGPT HTTPS hosts and respect cookie domain, path and expiry. | [Codex cookie policy](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/http-client/src/chatgpt_cloudflare_cookies.rs), [trusted hosts](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/http-client/src/chatgpt_hosts.rs) | Cookie allowlist, host/domain/path/expiry, concurrent access, client isolation and redirect tests. |
| Forward native model/reasoning markers, rate limits, credits and safety-buffering metadata. | [Codex response metadata](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/codex-api/src/sse/responses.rs#L32), [rate limits](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/codex-api/src/rate_limits.rs), [safety buffering](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/codex-api/src/safety_buffering.rs) | Response metadata tests, including errors and an empty reasoning marker. |
| Keep native reasoning-summary stream options and exact JSON schema numbers. Carry cache settings through Chat Completions conversion too. | [Codex stream options](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/codex-api/src/common.rs#L173), [raw tools representation](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/codex-api/src/common.rs#L228) | Native stream options, numeric schema and Chat cache-field regression tests. |
| Preserve ordered history and tools, cache settings, reasoning, text format/verbosity and service tier. Forward native response usage and diagnostic fields without reconstruction on the Responses path. | [Responses request construction](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/core/src/client.rs), [pi-mono Responses implementation](https://github.com/badlogic/pi-mono/blob/269afc66abfd5fdd37ff0eedfe2b749b6050a420/packages/ai/src/api/openai-codex-responses.ts) | Body-preservation and terminal response tests, including cached-token usage. |

The adapter supplies its own OAuth authorization and account identity. Caller
authorization, organization headers and cookies never select an upstream account.
Infrastructure cookies stay in memory and are never returned to callers.
The upstream model-catalog ETag is deliberately omitted because `/v1/models`
serves this adapter's configured allowlist, rather than that upstream catalog.

An intermediary must forward these native headers in both directions. A typed
gateway can also discard request or response fields before this adapter sees
them; preserving fields here cannot repair that loss. Verify the complete route,
particularly when using cache diagnostics or a Responses Lite request.

No cache key, retention option or diagnostic request is invented automatically.
The upstream may reject options or report diagnostics as unavailable. This
change does not add WebSockets, stateful response references, response retrieval,
or automatic account selection. Those capabilities remain outside the API
contract documented in the README.
