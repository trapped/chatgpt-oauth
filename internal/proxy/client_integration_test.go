package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestServerRetainsPrivateRoutingCookies(t *testing.T) {
	s := &Server{Auth: testAuth(t), Models: []string{"test-model"}}
	client := s.httpClient()
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.httpClient() != client {
				t.Error("server replaced upstream client")
			}
		}()
	}
	wg.Wait()
	calls := 0
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 && r.Header.Get("Cookie") != "" {
			t.Error("caller cookie reached upstream")
		}
		if calls == 2 && r.Header.Get("Cookie") != "__oailb=synthetic-route" {
			t.Error("routing cookie was not retained privately")
		}
		w := httptest.NewRecorder()
		w.Header().Add("Set-Cookie", "__oailb=synthetic-route; Secure; Path=/")
		w.Header().Add("Set-Cookie", "account_session=must-not-be-kept; Secure; Path=/")
		sse(w, terminal())
		resp := w.Result()
		resp.Request = r
		return resp, nil
	})
	for range 2 {
		r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"test-model","input":"hello"}`))
		r.Header.Set("Cookie", "caller=must-not-forward")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if w.Header().Get("Set-Cookie") != "" {
			t.Error("private cookie returned to caller")
		}
	}
	if calls != 2 {
		t.Fatalf("upstream calls=%d", calls)
	}
}

func TestServerHonorsInjectedClient(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	s := &Server{Client: client}
	if s.httpClient() != client {
		t.Fatal("injected HTTP client ignored")
	}
}
