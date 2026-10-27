package proxy

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultMaxBodyBytes matches Bifrost's bounded 100 MiB default, rather than
// imposing a smaller limit on native image data URLs and full-history requests.
// https://github.com/maximhq/bifrost/blob/a05ea20e68bf116a35e9f05b1a3d749624b3935e/transports/bifrost-http/lib/config.go#L846
const DefaultMaxBodyBytes int64 = 100 << 20

// Large or unknown-length bodies have bounded admission independently of the
// byte limit: JSON normalization can hold several representations in memory.
// This is an application memory policy, not an upstream protocol requirement.
const largeBodyThreshold int64 = 8 << 20

const requestBodyReadTimeout = 30 * time.Second

type Server struct {
	Auth           *Auth
	BaseURL        string
	Models         []string
	Token          string
	Client         *http.Client
	MaxBody        int64
	clientOnce     sync.Once
	upstreamClient *http.Client
	largeBodyOnce  sync.Once
	largeBodies    chan struct{}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" && r.URL.Path == "/health/live" {
		w.WriteHeader(200)
		return
	}
	if s.Token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.Token)) != 1 {
		writeError(w, 401, "invalid_api_key", "Invalid backend credential")
		return
	}
	if r.Method == "GET" && r.URL.Path == "/health/ready" {
		if _, err := s.Auth.Current(r.Context()); err != nil {
			writeError(w, 503, "authentication_error", err.Error())
			return
		}
		w.WriteHeader(200)
		return
	}
	if r.Method == "GET" && r.URL.Path == "/v1/models" {
		models := []any{}
		for _, m := range s.Models {
			models = append(models, map[string]any{"id": m, "object": "model", "created": 0, "owned_by": "chatgpt"})
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": models})
		return
	}
	chat := r.URL.Path == "/v1/chat/completions"
	if r.Method != "POST" || (!chat && r.URL.Path != "/v1/responses") {
		writeError(w, 404, "not_found", "Unknown endpoint")
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, 400, "invalid_request_error", "Query parameters are unsupported")
		return
	}
	limit := s.MaxBody
	if limit == 0 {
		limit = DefaultMaxBodyBytes
	}
	if limit < 0 {
		writeError(w, 500, "server_error", "Invalid request body limit")
		return
	}
	if r.ContentLength > limit {
		writeError(w, 413, "invalid_request_error", fmt.Sprintf("Request body exceeds the configured limit of %d bytes", limit))
		return
	}
	controller := http.NewResponseController(w)
	setReadDeadline := func(deadline time.Time) bool {
		if err := controller.SetReadDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			writeError(w, 400, "invalid_request_error", "Cannot set request body deadline")
			return false
		}
		return true
	}
	if r.ContentLength < 0 || r.ContentLength > largeBodyThreshold {
		// ReadTimeout starts before admission. Clear it while queued: an expired
		// deadline cannot reliably be extended by ResponseController afterward.
		if !setReadDeadline(time.Time{}) {
			return
		}
		s.largeBodyOnce.Do(func() { s.largeBodies = make(chan struct{}, 1) })
		select {
		case s.largeBodies <- struct{}{}:
			defer func() { <-s.largeBodies }()
		case <-r.Context().Done():
			writeError(w, 408, "invalid_request_error", "Request canceled while waiting for body capacity")
			return
		}
	}
	// The body gets the full read window after admission, independently of any
	// preceding response stream. Clear it after reading, before our own SSE.
	if !setReadDeadline(time.Now().Add(requestBodyReadTimeout)) {
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	_ = controller.SetReadDeadline(time.Time{})
	if err != nil {
		var oversized *http.MaxBytesError
		if errors.As(err, &oversized) {
			writeError(w, 413, "invalid_request_error", fmt.Sprintf("Request body exceeds the configured limit of %d bytes", limit))
		} else {
			writeError(w, 400, "invalid_request_error", "Cannot read request body")
		}
		return
	}
	var body map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	// Preserve numeric schema values exactly, as native Codex does with raw tools.
	decoder.UseNumber()
	if decoder.Decode(&body) != nil || body == nil || decoder.Decode(new(any)) != io.EOF {
		writeError(w, 400, "invalid_request_error", "Expected a JSON object")
		return
	}
	model, _ := body["model"].(string)
	if !contains(s.Models, model) {
		writeError(w, 400, "model_not_found", "Model is not configured")
		return
	}
	stream, _ := body["stream"].(bool)
	includeUsage := false
	if opt, ok := body["stream_options"].(map[string]any); ok {
		includeUsage, _ = opt["include_usage"].(bool)
	}
	if chat {
		body, err = chatRequest(body)
	}
	if err == nil {
		err = normalize(body)
	}
	if err != nil {
		writeError(w, 400, "invalid_request_error", err.Error())
		return
	}
	c, err := s.Auth.Current(r.Context())
	if err != nil {
		writeError(w, 503, "authentication_error", err.Error())
		return
	}
	var payload bytes.Buffer
	encoder := json.NewEncoder(&payload)
	// This is a JSON protocol body, not JSON embedded in HTML. Avoid expanding
	// each <, > or & into a six-byte escape in large text/history requests.
	// https://pkg.go.dev/encoding/json#Encoder.SetEscapeHTML
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(body); err != nil {
		writeError(w, 500, "server_error", "Cannot encode upstream request")
		return
	}
	data = bytes.TrimSuffix(payload.Bytes(), []byte("\n"))
	base := s.BaseURL
	if base == "" {
		base = "https://chatgpt.com/backend-api/codex"
	}
	upstream, err := http.NewRequestWithContext(r.Context(), "POST", strings.TrimRight(base, "/")+"/responses", bytes.NewReader(data))
	if err != nil {
		writeError(w, 500, "server_error", "Invalid upstream URL")
		return
	}
	setUpstreamHeaders(upstream.Header, r.Header, body, c)
	resp, err := s.httpClient().Do(upstream)
	if err != nil {
		writeError(w, 502, "upstream_error", "Upstream request failed")
		return
	}
	defer resp.Body.Close()
	copyUpstreamHeaders(w.Header(), resp.Header)
	if resp.StatusCode != 200 {
		// Never reflect upstream bodies, which may contain credentials or request text.
		status := resp.StatusCode
		if status < 400 || status > 599 {
			status = 502
		}
		writeError(w, status, "upstream_error", fmt.Sprintf("Upstream returned HTTP %d", resp.StatusCode))
		return
	}
	cw := newChatWriter(w, model, includeUsage)
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
	}
	terminal := false
	err = readSSE(resp.Body, func(event map[string]any, raw []byte) error {
		typ, _ := event["type"].(string)
		if typ == "error" || typ == "response.failed" {
			return errors.New("upstream inference failed")
		}
		if typ == "response.completed" || typ == "response.incomplete" {
			result, ok := event["response"].(map[string]any)
			if !ok {
				return errors.New("invalid terminal response")
			}
			if !validUsage(result["usage"]) {
				return errors.New("upstream omitted usage")
			}
		}
		if stream {
			if chat {
				if e := cw.event(event); e != nil {
					return e
				}
			} else {
				if _, e := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, raw); e != nil {
					return e
				}
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if typ == "response.completed" || typ == "response.incomplete" {
			result, ok := event["response"].(map[string]any)
			if !ok {
				return errors.New("invalid terminal response")
			}
			if !validUsage(result["usage"]) {
				return errors.New("upstream omitted usage")
			}
			terminal = true
			if !stream {
				if chat {
					result = chatResponse(result, model)
				}
				writeJSON(w, 200, result)
			}
			return errComplete
		}
		return nil
	})
	if errors.Is(err, errComplete) && terminal {
		if stream && chat {
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		return
	}
	if stream {
		event := map[string]any{"error": map[string]any{"type": "upstream_error", "message": "Upstream stream failed or ended without completion and usage"}}
		b, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", b)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	writeError(w, 502, "upstream_error", "Upstream stream failed or ended without completion and usage")
}

func validUsage(value any) bool {
	u, ok := value.(map[string]any)
	if !ok {
		return false
	}
	counts := map[string]float64{}
	for _, key := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		n, ok := u[key].(float64)
		if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || math.Trunc(n) != n {
			return false
		}
		counts[key] = n
	}
	if counts["total_tokens"] != counts["input_tokens"]+counts["output_tokens"] {
		return false
	}
	for _, detail := range []struct{ field, count, parent string }{{"input_tokens_details", "cached_tokens", "input_tokens"}, {"output_tokens_details", "reasoning_tokens", "output_tokens"}} {
		if raw := u[detail.field]; raw != nil {
			m, ok := raw.(map[string]any)
			if !ok {
				return false
			}
			if value := m[detail.count]; value != nil {
				n, ok := value.(float64)
				if !ok || n < 0 || math.Trunc(n) != n || n > counts[detail.parent] {
					return false
				}
			}
		}
	}
	return true
}

func contains(values []string, v string) bool {
	for _, s := range values {
		if v == s {
			return true
		}
	}
	return false
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, kind, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"type": kind, "message": message}})
}

var errComplete = errors.New("complete")

func readSSE(r io.Reader, emit func(map[string]any, []byte) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	var data []string
	dispatch := func() error {
		if len(data) == 0 {
			return nil
		}
		raw := []byte(strings.Join(data, "\n"))
		data = nil
		if string(raw) == "[DONE]" {
			return nil
		}
		var e map[string]any
		if json.Unmarshal(raw, &e) != nil || e == nil {
			return errors.New("invalid upstream event")
		}
		return emit(e, raw)
	}
	size := 0
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			size = 0
			continue
		}
		if strings.HasPrefix(line, "data:") {
			part := strings.TrimPrefix(line, "data:")
			part = strings.TrimPrefix(part, " ")
			size += len(part)
			if size > 4<<20 {
				return errors.New("upstream event too large")
			}
			data = append(data, part)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return dispatch()
}

func normalize(b map[string]any) error {
	if v := b["previous_response_id"]; v != nil {
		return errors.New("previous_response_id is unsupported; send full conversation history")
	}
	if input, ok := b["input"].([]any); ok {
		for _, item := range input {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if m["type"] == "item_reference" {
				return errors.New("item_reference is unsupported; send full conversation history")
			}
			// Some OpenAI-compatible gateways strip status from replayed native
			// search calls. The subscription Responses endpoint requires it, even
			// though id is optional. These are prior calls in stateless history;
			// preserve explicit statuses and all remaining fields verbatim.
			if m["type"] == "web_search_call" && m["status"] == nil {
				m["status"] = "completed"
			}
		}
	}
	if text, ok := b["input"].(string); ok {
		b["input"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}
	}
	if _, ok := b["input"].([]any); !ok {
		return errors.New("input must be a string or array")
	}
	if b["instructions"] == nil {
		b["instructions"] = ""
	}
	b["store"] = false
	b["stream"] = true
	include, _ := b["include"].([]any)
	found := false
	for _, v := range include {
		if v == "reasoning.encrypted_content" {
			found = true
		}
	}
	if !found {
		include = append(include, "reasoning.encrypted_content")
	}
	b["include"] = include
	// Native Responses stream_options includes reasoning_summary_delivery.
	// Chat Completions include_usage is consumed by chatRequest instead.
	// https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/codex-api/src/common.rs#L173-L181
	for _, key := range []string{"max_output_tokens", "max_tokens", "temperature", "top_p", "presence_penalty", "frequency_penalty", "logprobs", "top_logprobs", "user"} {
		delete(b, key)
	}
	return nil
}

func (s *Server) httpClient() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	s.clientOnce.Do(func() { s.upstreamClient = newUpstreamClient() })
	return s.upstreamClient
}
