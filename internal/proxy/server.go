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
	"time"
)

type Server struct {
	Auth    *Auth
	BaseURL string
	Models  []string
	Token   string
	Client  *http.Client
	MaxBody int64
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
		limit = 8 << 20
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		writeError(w, 413, "invalid_request_error", "Request body too large")
		return
	}
	var body map[string]any
	if json.Unmarshal(data, &body) != nil || body == nil {
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
	data, _ = json.Marshal(body)
	base := s.BaseURL
	if base == "" {
		base = "https://chatgpt.com/backend-api/codex"
	}
	upstream, err := http.NewRequestWithContext(r.Context(), "POST", strings.TrimRight(base, "/")+"/responses", bytes.NewReader(data))
	if err != nil {
		writeError(w, 500, "server_error", "Invalid upstream URL")
		return
	}
	upstream.Header.Set("Authorization", "Bearer "+c.AccessToken)
	upstream.Header.Set("ChatGPT-Account-ID", c.AccountID)
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Accept", "text/event-stream")
	upstream.Header.Set("Originator", "codex_cli_rs")
	upstream.Header.Set("User-Agent", "chatgpt-oauth/0.1")
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(upstream)
	if err != nil {
		writeError(w, 502, "upstream_error", "Upstream request failed")
		return
	}
	defer resp.Body.Close()
	for _, name := range []string{"Retry-After", "X-Request-ID", "X-Codex-Primary-Used-Percent", "X-Codex-Primary-Reset-After-Seconds", "X-Codex-Secondary-Used-Percent", "X-Codex-Secondary-Reset-After-Seconds"} {
		if v := resp.Header.Get(name); v != "" {
			w.Header().Set(name, v)
		}
	}
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
			if m, ok := item.(map[string]any); ok && m["type"] == "item_reference" {
				return errors.New("item_reference is unsupported; send full conversation history")
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
	for _, key := range []string{"max_output_tokens", "max_tokens", "temperature", "top_p", "presence_penalty", "frequency_penalty", "logprobs", "top_logprobs", "user", "stream_options"} {
		delete(b, key)
	}
	return nil
}
