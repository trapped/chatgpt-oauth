package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultBodyLimitAcceptsLargeNativeImage(t *testing.T) {
	// A valid, uncompressed PNG keeps the fixture deterministic and above 8 MiB
	// once base64-encoded, without checking a large binary artifact into Git.
	var encoded bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.NoCompression}
	if err := encoder.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 1280, 1280))); err != nil {
		t.Fatal(err)
	}
	imageURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(encoded.Bytes())
	wantHash := sha256.Sum256([]byte(imageURL))
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Input []struct {
				Content []struct {
					ImageURL string `json:"image_url"`
				} `json:"content"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Input) != 1 || len(body.Input[0].Content) != 2 {
			t.Errorf("invalid native image request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if gotHash := sha256.Sum256([]byte(body.Input[0].Content[1].ImageURL)); gotHash != wantHash {
			t.Error("native image bytes changed")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		sse(w, terminal())
	}))
	defer upstream.Close()
	body, err := json.Marshal(map[string]any{
		"model": "test-model",
		"input": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "Describe this synthetic image."},
			map[string]any{"type": "input_image", "image_url": imageURL},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) <= 8<<20 || int64(len(body)) >= DefaultMaxBodyBytes {
		t.Fatalf("fixture size %d does not exercise the former 8 MiB limit", len(body))
	}
	server := &Server{Auth: testAuth(t), BaseURL: upstream.URL, Models: []string{"test-model"}}
	result := httptest.NewRecorder()
	server.ServeHTTP(result, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)))
	if result.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("large native image failed: status %d, upstream calls %d, response %s", result.Code, calls.Load(), result.Body.String())
	}
}

func TestConfiguredBodyLimitExactBoundary(t *testing.T) {
	const body = `{"model":"test-model","input":"boundary"}`
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		sse(w, terminal())
	}))
	defer upstream.Close()
	for _, chunked := range []bool{false, true} {
		for _, oversized := range []bool{false, true} {
			limit := int64(len(body))
			want := http.StatusOK
			if oversized {
				limit--
				want = http.StatusRequestEntityTooLarge
			}
			server := &Server{Auth: testAuth(t), BaseURL: upstream.URL, Models: []string{"test-model"}, MaxBody: limit}
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
			if chunked {
				request.ContentLength = -1
				request.TransferEncoding = []string{"chunked"}
			}
			result := httptest.NewRecorder()
			server.ServeHTTP(result, request)
			if result.Code != want {
				t.Errorf("chunked=%v oversized=%v: status %d, want %d", chunked, oversized, result.Code, want)
			}
			if oversized && !strings.Contains(result.Body.String(), "configured limit") {
				t.Error("413 omitted the configured limit")
			}
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("oversize request reached upstream: %d calls", calls.Load())
	}
}

type brokenRequestBody struct{}

func (brokenRequestBody) Read([]byte) (int, error) {
	return 0, errors.New("sensitive synthetic failure")
}
func (brokenRequestBody) Close() error { return nil }

func TestBodyReadFailureIsNotSizeError(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.Body = brokenRequestBody{}
	result := httptest.NewRecorder()
	(&Server{}).ServeHTTP(result, request)
	if result.Code != http.StatusBadRequest || strings.Contains(result.Body.String(), "sensitive") || strings.Contains(result.Body.String(), "too large") {
		t.Fatalf("body read failure misclassified or leaked: %d %s", result.Code, result.Body.String())
	}
}

type heldRequestBody struct {
	entered chan struct{}
	release chan struct{}
	reader  io.Reader
	started bool
}

func (b *heldRequestBody) Read(p []byte) (int, error) {
	if !b.started {
		b.started = true
		close(b.entered)
		<-b.release
	}
	return b.reader.Read(p)
}
func (b *heldRequestBody) Close() error { return nil }

func TestLargeBodyAdmissionAllowsSmallRequestsAndCancellation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sse(w, terminal()) }))
	defer upstream.Close()
	server := &Server{Auth: testAuth(t), BaseURL: upstream.URL, Models: []string{"test-model"}}
	const body = `{"model":"test-model","input":"admission"}`
	held := &heldRequestBody{entered: make(chan struct{}), release: make(chan struct{}), reader: strings.NewReader(body)}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.Body = held
	request.ContentLength = largeBodyThreshold + 1
	firstResult := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.ServeHTTP(firstResult, request)
	}()
	select {
	case <-held.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("large request did not enter body processing")
	}
	defer func() {
		close(held.release)
		select {
		case <-done:
			if firstResult.Code != http.StatusOK {
				t.Errorf("admitted large request failed: %d", firstResult.Code)
			}
		case <-time.After(5 * time.Second):
			t.Error("large request did not release capacity")
		}
	}()
	// A known small request can complete while the large body owns its slot.
	smallResult := httptest.NewRecorder()
	smallDone := make(chan struct{})
	go func() {
		defer close(smallDone)
		server.ServeHTTP(smallResult, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	}()
	select {
	case <-smallDone:
	case <-time.After(5 * time.Second):
		t.Fatal("small request waited for large-body capacity")
	}
	if smallResult.Code != http.StatusOK {
		t.Fatalf("small request blocked or failed: %d", smallResult.Code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	waiting := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)).WithContext(ctx)
	waiting.ContentLength = -1
	cancel()
	canceledResult := httptest.NewRecorder()
	canceledDone := make(chan struct{})
	go func() {
		defer close(canceledDone)
		server.ServeHTTP(canceledResult, waiting)
	}()
	select {
	case <-canceledDone:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled request did not leave the capacity queue")
	}
	if canceledResult.Code != http.StatusRequestTimeout {
		t.Fatalf("canceled queued request status = %d", canceledResult.Code)
	}
}
