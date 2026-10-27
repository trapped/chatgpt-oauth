package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// The native HTTP request controls are defined by Codex's ResponsesApiRequest,
// Reasoning and StreamOptions at this pinned revision:
// https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/codex-api/src/common.rs#L140
// Cache diagnostics are an opt-in pass-through, never enabled by the adapter:
// https://developers.openai.com/api/docs/guides/prompt-caching/diagnostics
// Retention and future_* sentinels below test opaque forwarding of caller fields;
// they do not assert that a particular OAuth account/model accepts those fields.
const cacheSensitiveResponsesBody = `{
  "model": "test-model",
  "instructions": "Stable instructions.\nPreserve every earlier item, including whitespace.",
  "input": [
    {"type":"message","role":"developer","content":[{"type":"input_text","text":"Stable developer prefix."}]},
    {"type":"message","role":"user","content":[{"type":"input_text","text":"First request.\n"}]},
    {"type":"reasoning","id":"rs_prior","encrypted_content":"opaque-prior-reasoning","summary":[{"type":"summary_text","text":"Prior reasoning summary."}]},
    {"type":"function_call","id":"fc_prior","call_id":"call_prior","name":"z_lookup","arguments":"{ \"query\": \"cache\", \"limit\": 2 }"},
    {"type":"function_call_output","call_id":"call_prior","output":"  first result\nsecond result\n"},
    {"type":"web_search_call","id":"ws_prior","status":"completed","action":{"type":"search","queries":["cache affinity","routing cookies"]}},
    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"Earlier answer.","annotations":[]}]},
    {"type":"message","role":"user","content":[{"type":"input_text","text":"Continue."}]}
  ],
  "tools": [
    {"type":"function","name":"z_lookup","description":"First tool, intentionally not alphabetical.","parameters":{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query","limit"],"additionalProperties":false},"strict":true},
    {"type":"function","name":"a_finish","description":"Second tool.","parameters":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false},"strict":true}
  ],
  "tool_choice": "auto",
  "parallel_tool_calls": true,
  "reasoning": {"effort":"medium","summary":"auto","context":"all_turns"},
  "text": {"verbosity":"low","format":{"type":"json_schema","name":"codex_output_schema","strict":true,"schema":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}}},
  "prompt_cache_key": "stable-conversation-key",
  "prompt_cache_options": {"comparison_response_id":"resp_previous"},
  "prompt_cache_retention": "in_memory",
  "service_tier": "default",
  "stream_options": {"reasoning_summary_delivery":"sequential_cutoff"},
  "store": false,
  "stream": true,
  "include": ["web_search_call.action.sources","reasoning.encrypted_content"],
  "client_metadata": {"session_id":"stable-conversation-key"},
  "future_request_field": {"ordered_values":["z","a"],"enabled":true}
}`

func decodeParityBody(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func marshalParityBody(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertParityItemHashes(t *testing.T, field string, got, want any) {
	t.Helper()
	actual, ok := got.([]any)
	if !ok {
		t.Fatalf("%s is not an array", field)
	}
	expected := want.([]any)
	if len(actual) != len(expected) {
		t.Fatalf("%s length = %d, want %d", field, len(actual), len(expected))
	}
	for index := range expected {
		actualHash := sha256.Sum256(marshalParityBody(t, actual[index]))
		expectedHash := sha256.Sum256(marshalParityBody(t, expected[index]))
		if actualHash != expectedHash {
			t.Errorf("%s[%d] changed: hash %x, want %x", field, index, actualHash, expectedHash)
		}
	}
}

func TestResponsesNormalizationPreservesCacheSensitiveFields(t *testing.T) {
	before := decodeParityBody(t, []byte(cacheSensitiveResponsesBody))
	after := decodeParityBody(t, []byte(cacheSensitiveResponsesBody))
	if err := normalize(after); err != nil {
		t.Fatal(err)
	}
	assertParityItemHashes(t, "input", after["input"], before["input"])
	assertParityItemHashes(t, "tools", after["tools"], before["tools"])
	for field, expected := range before {
		if !reflect.DeepEqual(after[field], expected) {
			t.Errorf("caller field %q changed: got %#v, want %#v", field, after[field], expected)
		}
	}
	if len(after) != len(before) {
		t.Errorf("normalization changed the field count of an already normalized request: got %d, want %d", len(after), len(before))
	}
}

func TestResponsesNormalizationIsIdempotent(t *testing.T) {
	for _, data := range []string{
		`{"model":"test-model","input":"Stable input.","prompt_cache_key":"stable-key","include":["web_search_call.action.sources"],"store":true,"stream":false}`,
		`{"model":"test-model","input":[{"type":"web_search_call","action":{"type":"search","query":"cache"}},{"type":"message","role":"user","content":[{"type":"input_text","text":"Continue."}]}],"include":[]}`,
		cacheSensitiveResponsesBody,
	} {
		body := decodeParityBody(t, []byte(data))
		if err := normalize(body); err != nil {
			t.Fatal(err)
		}
		once := marshalParityBody(t, body)
		if err := normalize(body); err != nil {
			t.Fatal(err)
		}
		if twice := marshalParityBody(t, body); !bytes.Equal(once, twice) {
			t.Fatalf("normalization changed on second pass:\nfirst: %s\nsecond: %s", once, twice)
		}
		if body["store"] != false || body["stream"] != true {
			t.Error("subscription Responses transport requirements were not applied")
		}
		count := 0
		for _, include := range body["include"].([]any) {
			if include == "reasoning.encrypted_content" {
				count++
			}
		}
		if count != 1 {
			t.Errorf("encrypted reasoning include count = %d, want 1", count)
		}
	}
}

func TestResponsesServerPreservesBodyUsageAndDiagnostics(t *testing.T) {
	// "unavailable" is a documented diagnostics result and the result observed on
	// our OAuth upstream. No diagnostics are synthesized or requested by default.
	response := decodeParityBody(t, []byte(`{
      "id":"resp_current","object":"response","status":"completed","model":"test-model","service_tier":"default",
      "output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK","annotations":[]}]}],
      "usage":{"input_tokens":149240,"input_tokens_details":{"cached_tokens":3712},"output_tokens":1000,"output_tokens_details":{"reasoning_tokens":700},"total_tokens":150240},
      "prompt_cache_diagnostics":{"type":"unavailable"},
      "future_response_field":{"ordered_values":["z","a"]}
    }`))
	event := map[string]any{"type": "response.completed", "response": response}
	captured := make(chan []byte, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		captured <- data
		w.Header().Set("Content-Type", "text/event-stream")
		sse(w, event)
	}))
	defer upstream.Close()
	server := &Server{Auth: testAuth(t), BaseURL: upstream.URL, Models: []string{"test-model"}}
	expectedRequest := decodeParityBody(t, []byte(cacheSensitiveResponsesBody))
	var firstRequestHash [sha256.Size]byte
	for _, stream := range []bool{false, true} {
		for repeat := range 2 {
			t.Run(fmt.Sprintf("stream=%v/repeat=%d", stream, repeat), func(t *testing.T) {
				body := decodeParityBody(t, []byte(cacheSensitiveResponsesBody))
				body["stream"] = stream
				request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(marshalParityBody(t, body)))
				result := httptest.NewRecorder()
				server.ServeHTTP(result, request)
				if result.Code != http.StatusOK {
					t.Fatalf("status %d: %s", result.Code, result.Body.String())
				}
				requestData := <-captured
				upstreamBody := decodeParityBody(t, requestData)
				assertParityItemHashes(t, "input", upstreamBody["input"], expectedRequest["input"])
				assertParityItemHashes(t, "tools", upstreamBody["tools"], expectedRequest["tools"])
				for field, expected := range expectedRequest {
					if !reflect.DeepEqual(upstreamBody[field], expected) {
						t.Errorf("upstream request field %q changed", field)
					}
				}
				if len(upstreamBody) != len(expectedRequest) {
					t.Errorf("upstream request field count = %d, want %d", len(upstreamBody), len(expectedRequest))
				}
				requestHash := sha256.Sum256(requestData)
				if firstRequestHash == ([sha256.Size]byte{}) {
					firstRequestHash = requestHash
				} else if requestHash != firstRequestHash {
					t.Error("identical requests generated changing upstream bodies")
				}
				var actualResponse map[string]any
				if stream {
					for _, line := range strings.Split(result.Body.String(), "\n") {
						if strings.HasPrefix(line, "data: ") {
							actualEvent := decodeParityBody(t, []byte(strings.TrimPrefix(line, "data: ")))
							if actualEvent["type"] == "response.completed" {
								actualResponse, _ = actualEvent["response"].(map[string]any)
							}
						}
					}
				} else {
					actualResponse = decodeParityBody(t, result.Body.Bytes())
				}
				if !reflect.DeepEqual(actualResponse, response) {
					t.Errorf("response, exact usage or diagnostics changed: got %#v, want %#v", actualResponse, response)
				}
			})
		}
	}
}

func TestResponsesServerPreservesExactSchemaNumbers(t *testing.T) {
	// Native Codex holds tool definitions as raw JSON (ResponsesApiTools at the
	// source revision above), so a proxy must not round schema enum/const values
	// by decoding them through float64. Both examples are valid uint64 values.
	captured := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		captured <- data
		w.Header().Set("Content-Type", "text/event-stream")
		sse(w, terminal())
	}))
	defer upstream.Close()
	server := &Server{Auth: testAuth(t), BaseURL: upstream.URL, Models: []string{"test-model"}}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
      "model":"test-model","input":"Preserve the schema values.",
      "tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"identifier":{"type":"integer","enum":[9007199254740993]}}}}],
      "text":{"format":{"type":"json_schema","name":"exact_identifier","schema":{"type":"object","properties":{"identifier":{"type":"integer","const":18446744073709551615}}}}}
    }`))
	result := httptest.NewRecorder()
	server.ServeHTTP(result, request)
	if result.Code != http.StatusOK {
		t.Fatalf("status %d: %s", result.Code, result.Body.String())
	}
	data := <-captured
	for _, literal := range []string{`"enum":[9007199254740993]`, `"const":18446744073709551615`} {
		if !bytes.Contains(data, []byte(literal)) {
			t.Errorf("schema numeric literal %s was changed: %s", literal, data)
		}
	}
}

func TestChatConversionPreservesExplicitCacheControls(t *testing.T) {
	body := decodeParityBody(t, []byte(`{
      "model":"test-model","messages":[{"role":"user","content":"Stable conversation text."}],
      "prompt_cache_key":"stable-key",
      "prompt_cache_options":{"comparison_response_id":"resp_previous"},
      "prompt_cache_retention":"in_memory"
    }`))
	converted, err := chatRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := normalize(converted); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"prompt_cache_key", "prompt_cache_options", "prompt_cache_retention"} {
		if !reflect.DeepEqual(converted[field], body[field]) {
			t.Errorf("explicit Chat cache control %q changed: got %#v, want %#v", field, converted[field], body[field])
		}
	}
}

func TestChatChoiceCountValidationWithJSONNumbers(t *testing.T) {
	for _, count := range []string{"1", "2"} {
		t.Run(count, func(t *testing.T) {
			body := map[string]any{
				"model": "test-model", "n": json.Number(count),
				"messages": []any{map[string]any{"role": "user", "content": "Stable input."}},
			}
			_, err := chatRequest(body)
			if (err == nil) != (count == "1") {
				t.Errorf("choice count %s: error = %v", count, err)
			}
		})
	}
}
