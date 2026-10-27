# chatgpt-oauth

A small Go service exposing ChatGPT subscription inference through an
OpenAI-compatible API. Standard library only; no database or gateway policies.

## Run

```sh
go build -o chatgpt-oauth ./cmd/chatgpt-oauth
mkdir -m 700 -p /private/chatgpt-oauth
./chatgpt-oauth login --auth-file /private/chatgpt-oauth/auth.json
./chatgpt-oauth serve --auth-file /private/chatgpt-oauth/auth.json --models YOUR_MODEL
```

Authorize the short-lived device code in your browser. The server listens on
loopback port 10531 by default. Configure the explicit model allowlist with
`--models`. A non-loopback listener requires `BACKEND_TOKEN_FILE`, pointing to a
readable mounted backend credential file. Do not publish this service directly.

Request bodies are limited to **100 MiB (104,857,600 bytes)** by default. This is
a local bounded limit matching Bifrost's default, not an upstream acceptance
guarantee. Full conversation histories can include base64 image data URLs, so
an encoded request can be much larger than its text alone. Configure a positive
decimal byte count with `--max-body-bytes` or `CHATGPT_OAUTH_MAX_BODY_BYTES`;
the command-line flag takes precedence. Zero, negative and invalid values fail
startup. Keep intermediary limits and container memory budgets consistent with
the chosen request size; the adapter reads and normalizes the bounded JSON body
in memory. Requests over the configured limit return HTTP 413, while body-read
failures return HTTP 400. One request larger than 8 MiB or with an unknown body
length is admitted at a time, including its upstream response stream, to bound
normalization memory. Known smaller requests do not wait for that slot. A queued
request exits when its context is canceled. This admission policy is local to
the adapter and does not describe an upstream concurrency requirement.

## API

- `POST /v1/responses`: JSON and SSE.
- `POST /v1/chat/completions`: JSON and SSE, function tools and tool results.
- `GET /v1/models`: configured model allowlist.
- `GET /health/live`: process liveness, without touching credentials.
- `GET /health/ready`: authenticated credential readiness and refresh.

Credentials are stored in a dedicated file with atomic mode-0600 writes.
One process owns the file; refresh is serialized and rotated tokens are persisted.
`import-auth --from PATH --auth-file PATH` accepts flat or nested credential files.
Do not share refresh credentials with another running application.

## Limitations

Stateless: send full conversation history. No previous-response references,
retrieval/deletion, WebSockets, embeddings, audio or image generation. Only
function tools are translated for Chat Completions. Not all upstream parameters
are supported; output limits are not hard ceilings. Full OpenAI compatibility
is not claimed. A failed or unmetered stream is returned as an error.

## Gateway integration

Configure an OpenAI-compatible gateway with this service as its private backend.
The gateway must own client authentication, budgets, limits and public access.
Keep credentials and deployment configuration outside this repository and image.
The container runs as UID/GID 65532; mounted state must be writable by that user.

Native session affinity, turn state and client metadata are preserved through
the HTTP path. Infrastructure routing cookies remain private to the upstream
client. See the [protocol source map](docs/protocol-parity.md) for pinned sources,
verification and intermediary requirements.

## Tests

```sh
go vet ./...
go test -race ./...
docker build -t chatgpt-oauth:test .
```

Tests use synthetic credentials and mock upstream HTTP servers only.
Private deployments, rehearsals, benchmarks, operational reports and live test
artifacts do not belong in this repository.

## References

Independent implementation informed by the protocol handling in LiteLLM and
[EvanZhouDev/openai-oauth](https://github.com/EvanZhouDev/openai-oauth).
This is not an official OpenAI service.
