package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func jwt(exp int64) string {
	b, _ := json.Marshal(map[string]any{"exp": exp, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "test-account"}})
	return "test." + base64.RawURLEncoding.EncodeToString(b) + ".test"
}
func testAuth(t *testing.T) *Auth {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := SaveCredentials(path, Credentials{AccessToken: jwt(time.Now().Add(time.Hour).Unix()), RefreshToken: "synthetic-refresh", AccountID: "test-account"}); err != nil {
		t.Fatal(err)
	}
	return &Auth{Path: path}
}
func terminal() map[string]any {
	return map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_test", "object": "response", "status": "completed", "model": "test-model", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "OK"}}}}, "usage": map[string]any{"input_tokens": 100, "input_tokens_details": map[string]any{"cached_tokens": 40}, "output_tokens": 20, "output_tokens_details": map[string]any{"reasoning_tokens": 10}, "total_tokens": 120}}}
}
func sse(w io.Writer, v any) { b, _ := json.Marshal(v); fmt.Fprintf(w, "data: %s\n\n", b) }
func TestRefreshSerializedAndPersistent(t *testing.T) {
	auth := testAuth(t)
	_ = SaveCredentials(auth.Path, Credentials{AccessToken: jwt(1), RefreshToken: "synthetic-old", AccountID: "test-account"})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = r.ParseForm()
		if r.Form.Get("refresh_token") != "synthetic-old" {
			t.Error("wrong refresh token")
		}
		writeJSON(w, 200, map[string]any{"access_token": jwt(time.Now().Add(time.Hour).Unix()), "refresh_token": "synthetic-rotated"})
	}))
	defer upstream.Close()
	auth.BaseURL = upstream.URL
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := auth.Current(context.Background())
			if err != nil || c.RefreshToken != "synthetic-rotated" {
				t.Errorf("refresh failed: %v", err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh calls %d", calls.Load())
	}
	restored, err := ReadCredentials(auth.Path)
	if err != nil || restored.RefreshToken != "synthetic-rotated" {
		t.Fatal("rotation not persisted")
	}
	info, _ := os.Stat(auth.Path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("unsafe permissions")
	}
}
func TestImportFlatAndNested(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprint(nested), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "auth.json")
			var v any = Credentials{AccessToken: jwt(time.Now().Add(time.Hour).Unix())}
			if nested {
				v = map[string]any{"tokens": v}
			}
			b, _ := json.Marshal(v)
			_ = os.WriteFile(path, b, 0600)
			c, err := ReadCredentials(path)
			if err != nil || c.AccountID != "test-account" || c.ExpiresAt == 0 {
				t.Fatalf("import failed: %v", err)
			}
		})
	}
}
func TestAPIPathsAndUsage(t *testing.T) {
	for _, chat := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("chat=%v/stream=%v", chat, stream), func(t *testing.T) {
				auth := testAuth(t)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/responses" || r.Header.Get("ChatGPT-Account-ID") != "test-account" {
						t.Error("incorrect upstream request")
					}
					if r.Header.Get("Authorization") == "Bearer client-key" {
						t.Error("client credential leaked")
					}
					var b map[string]any
					_ = json.NewDecoder(r.Body).Decode(&b)
					if b["store"] != false || b["stream"] != true {
						t.Error("normalization missing")
					}
					if _, ok := b["max_output_tokens"]; ok {
						t.Error("unsupported output cap forwarded")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					sse(w, map[string]any{"type": "response.output_text.delta", "delta": "OK"})
					sse(w, terminal())
				}))
				defer upstream.Close()
				server := &Server{Auth: auth, BaseURL: upstream.URL, Models: []string{"test-model"}, Token: "client-key"}
				path := "/v1/responses"
				body := map[string]any{"model": "test-model", "input": "hello", "stream": stream, "max_output_tokens": 20}
				if chat {
					path = "/v1/chat/completions"
					delete(body, "input")
					body["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
					body["stream_options"] = map[string]any{"include_usage": true}
				}
				b, _ := json.Marshal(body)
				req := httptest.NewRequest("POST", path, bytes.NewReader(b))
				req.Header.Set("Authorization", "Bearer client-key")
				w := httptest.NewRecorder()
				server.ServeHTTP(w, req)
				if w.Code != 200 {
					t.Fatalf("status %d: %s", w.Code, w.Body.String())
				}
				out := w.Body.String()
				if !strings.Contains(out, "cached_tokens") || !strings.Contains(out, "reasoning_tokens") {
					t.Fatal("lost usage detail")
				}
				if chat && !strings.Contains(out, "completion_tokens") {
					t.Fatal("missing chat usage")
				}
				if stream && chat && !strings.Contains(out, "[DONE]") {
					t.Fatal("missing terminal marker")
				}
			})
		}
	}
}
func TestTruncationAndMissingUsageNeverSucceed(t *testing.T) {
	for _, missing := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("missing=%v/stream=%v", missing, stream), func(t *testing.T) {
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if missing {
						e := terminal()
						delete(e["response"].(map[string]any), "usage")
						sse(w, e)
					} else {
						sse(w, map[string]any{"type": "response.output_text.delta", "delta": "partial"})
					}
				}))
				defer up.Close()
				s := &Server{Auth: testAuth(t), BaseURL: up.URL, Models: []string{"test-model"}}
				w := httptest.NewRecorder()
				s.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":%v}`, stream))))
				if stream {
					if strings.Contains(w.Body.String(), "[DONE]") || !strings.Contains(w.Body.String(), "upstream_error") {
						t.Fatal("false success")
					}
				} else if w.Code != 502 {
					t.Fatalf("expected failure: %d", w.Code)
				}
			})
		}
	}
}
func TestToolHistoryAndStreamingArguments(t *testing.T) {
	var b map[string]any
	_ = json.Unmarshal([]byte(`{"model":"test-model","messages":[{"role":"system","content":"instructions"},{"role":"assistant","content":null,"tool_calls":[{"id":"call1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call1","content":"result"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`), &b)
	converted, err := chatRequest(b)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(converted)
	if !bytes.Contains(data, []byte("function_call_output")) || !bytes.Contains(data, []byte("developer")) {
		t.Fatal("tool history lost")
	}
	w := httptest.NewRecorder()
	c := newChatWriter(w, "test-model", true)
	events := []map[string]any{{"type": "response.output_item.added", "item": map[string]any{"type": "function_call", "id": "item1", "call_id": "call1", "name": "lookup", "arguments": ""}}, {"type": "response.output_item.done", "item": map[string]any{"type": "function_call", "id": "item1", "arguments": "{\"x\":1}"}}, terminal()}
	for _, e := range events {
		if err := c.event(e); err != nil {
			t.Fatal(err)
		}
	}
	out := w.Body.String()
	if !strings.Contains(out, `\"x\":1`) || !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatal("tool arguments or finish lost")
	}
}
func TestBoundaryRejectsAndSanitizes(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(429)
		fmt.Fprint(w, "SENSITIVE-UPSTREAM-TEXT")
	}))
	defer up.Close()
	s := &Server{Auth: testAuth(t), BaseURL: up.URL, Models: []string{"test-model"}, Token: "test-backend"}
	for _, tc := range []struct {
		path, body, token string
		status            int
	}{{"/v1/responses", `{"model":"test-model","input":"hi"}`, "", 401}, {"/v1/responses", `{"model":"unknown","input":"hi"}`, "test-backend", 400}, {"/v1/responses", `{"model":"test-model","input":"hi","previous_response_id":"x"}`, "test-backend", 400}, {"/v1/responses", `{"model":"test-model","input":"hi"}`, "test-backend", 429}} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
		r.Header.Set("Authorization", "Bearer "+tc.token)
		s.ServeHTTP(w, r)
		if w.Code != tc.status || strings.Contains(w.Body.String(), "SENSITIVE") {
			t.Errorf("boundary failed: %d %s", w.Code, w.Body.String())
		}
	}
	if calls.Load() != 1 {
		t.Fatal("unexpected upstream calls")
	}
}
func TestClientCancellationReachesUpstream(t *testing.T) {
	entered := make(chan struct{})
	canceled := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sse(w, map[string]any{"type": "response.created"})
		w.(http.Flusher).Flush()
		close(entered)
		<-r.Context().Done()
		close(canceled)
	}))
	defer up.Close()
	s := httptest.NewServer(&Server{Auth: testAuth(t), BaseURL: up.URL, Models: []string{"test-model"}})
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "POST", s.URL+"/v1/responses", strings.NewReader(`{"model":"test-model","input":"hi","stream":true}`))
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-entered
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("upstream not canceled")
	}
}

func TestDeviceLoginAndRefreshFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			writeJSON(w, 200, map[string]any{"device_auth_id": "synthetic-device", "user_code": "TEST-CODE", "interval": 5})
		case "/api/accounts/deviceauth/token":
			writeJSON(w, 200, map[string]any{"authorization_code": "synthetic-code", "code_verifier": "synthetic-verifier"})
		case "/oauth/token":
			_ = r.ParseForm()
			if r.Form.Get("grant_type") == "refresh_token" {
				writeJSON(w, 401, map[string]any{"error": "SECRET-MUST-NOT-ESCAPE"})
				return
			}
			if r.Form.Get("code") != "synthetic-code" || r.Form.Get("code_verifier") != "synthetic-verifier" {
				t.Error("wrong code exchange")
			}
			writeJSON(w, 200, map[string]any{"access_token": jwt(time.Now().Add(time.Hour).Unix()), "refresh_token": "synthetic-refresh"})
		}
	}))
	defer up.Close()
	a := &Auth{Path: path, BaseURL: up.URL}
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.Login(ctx, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "TEST-CODE") {
		t.Fatal("missing device instructions")
	}
	c, err := ReadCredentials(path)
	if err != nil || c.RefreshToken != "synthetic-refresh" {
		t.Fatal("credentials not persisted")
	}
	c.ExpiresAt = 1
	_ = SaveCredentials(path, c)
	_, err = a.Current(ctx)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("refresh error leaked or failed open")
	}
}

func TestRotatedCredentialsRetryPersistenceWithoutRefreshingAgain(t *testing.T) {
	a := testAuth(t)
	a.cached = &Credentials{AccessToken: jwt(1), RefreshToken: "old", AccountID: "test-account", ExpiresAt: 1}
	var n atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		writeJSON(w, 200, map[string]any{"access_token": jwt(time.Now().Add(time.Hour).Unix()), "refresh_token": "rotated"})
	}))
	defer up.Close()
	a.BaseURL = up.URL
	directory := filepath.Join(t.TempDir(), "directory")
	_ = os.Mkdir(directory, 0700)
	a.Path = directory
	if _, err := a.Current(context.Background()); err == nil {
		t.Fatal("persistence failure must fail closed")
	}
	a.Path = filepath.Join(t.TempDir(), "auth.json")
	c, err := a.Current(context.Background())
	if err != nil || c.RefreshToken != "rotated" || n.Load() != 1 {
		t.Fatal("rotated credentials not retained safely")
	}
}

func TestMultimodalStructuredOutputContract(t *testing.T) {
	var body map[string]any
	_ = json.Unmarshal([]byte(`{"model":"test-model","messages":[{"role":"user","content":[{"type":"text","text":"Describe this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,c3ludGhldGlj","detail":"low"}}]}],"response_format":{"type":"json_schema","json_schema":{"name":"description","strict":true,"schema":{"type":"object","properties":{"description":{"type":"string"}},"required":["description"],"additionalProperties":false}}}}`), &body)
	converted, err := chatRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	content := converted["input"].([]any)[0].(map[string]any)["content"].([]any)
	image := content[1].(map[string]any)
	if image["type"] != "input_image" || image["detail"] != "low" || image["image_url"] != "data:image/png;base64,c3ludGhldGlj" {
		t.Fatal("image contract changed")
	}
	format := converted["text"].(map[string]any)["format"].(map[string]any)
	if format["name"] != "description" || format["strict"] != true || format["type"] != "json_schema" || format["schema"] == nil {
		t.Fatal("structured-output contract lost")
	}
}

func TestInvalidUsageIsRejected(t *testing.T) {
	for _, raw := range []string{`{}`, `{"input_tokens":100,"output_tokens":20,"total_tokens":0}`, `{"input_tokens":100,"output_tokens":-1,"total_tokens":99}`, `{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":101}}`} {
		var value any
		_ = json.Unmarshal([]byte(raw), &value)
		if validUsage(value) {
			t.Fatal("invalid accounting accepted")
		}
	}
	var value any
	_ = json.Unmarshal([]byte(`{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":40},"output_tokens_details":{"reasoning_tokens":10}}`), &value)
	if !validUsage(value) {
		t.Fatal("valid accounting rejected")
	}
}

// Exercise the actual adapter boundary: a gateway has omitted status from native
// search history, and the upstream validates it before completing a follow-up.
func TestNativeSearchHistoryRestoresMissingStatus(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			calls := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var b map[string]any
				if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
					t.Fatal(err)
				}
				input := b["input"].([]any)
				for i, action := range []string{"search", "open_page", "find_in_page"} {
					item := input[i].(map[string]any)
					if item["status"] != "completed" || item["action"].(map[string]any)["type"] != action {
						writeError(w, 400, "invalid_request_error", "Missing required parameter: status")
						t.Error("native search history not normalized")
						return
					}
					if item["id"] != nil {
						t.Error("invented native search ID")
					}
				}
				if input[2].(map[string]any)["action"].(map[string]any)["pattern"] != "cache" {
					t.Error("lost search action fields")
				}
				if input[3].(map[string]any)["status"] != "in_progress" {
					t.Error("explicit status changed")
				}
				if input[4].(map[string]any)["status"] != nil {
					t.Error("added status to another item type")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				sse(w, terminal())
			}))
			defer up.Close()
			server := &Server{Auth: testAuth(t), BaseURL: up.URL, Models: []string{"test-model"}}
			body := fmt.Sprintf(`{"model":"test-model","stream":%v,"input":[
    {"type":"web_search_call","action":{"type":"search","query":"cache"}},
    {"type":"web_search_call","action":{"type":"open_page","url":"https://example.com"}},
    {"type":"web_search_call","action":{"type":"find_in_page","url":"https://example.com","pattern":"cache"}},
    {"type":"web_search_call","status":"in_progress","action":{"type":"search","query":"cache"}},
    {"type":"message","role":"user","content":[{"type":"input_text","text":"Continue"}]}
   ]}`, stream)
			w := httptest.NewRecorder()
			server.ServeHTTP(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
			if calls != 1 || w.Code != 200 || !strings.Contains(w.Body.String(), `"completed"`) {
				t.Fatalf("follow-up failed: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
