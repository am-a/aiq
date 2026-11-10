package asks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientWireAndCleaning(t *testing.T) {
	const secret = "invented-secret-value"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer fixture-key" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("incorrect request method or headers")
		}
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), secret) || strings.Contains(string(b), `\u001b`) {
			t.Error("unclean input sent")
		}
		var body struct {
			State, Model string
			Questions    map[string]map[string]any
		}
		if err := json.Unmarshal(b, &body); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		q := body.Questions["test"]
		criteria, _ := q["criteria"].(map[string]any)
		if body.Model != "jev-latest" || len(body.State) > 8192 || q["type"] != "noul" || q["instructions"] == nil || q["question"] != nil || criteria["true"] == nil || criteria["false"] == nil {
			t.Error("incorrect wire contract")
		}
		io.WriteString(w, `{"answers":{"test":{"type":"noul","noul":0.75}}}`)
	}))
	defer server.Close()
	c := NewClient("fixture-key")
	c.Endpoint = server.URL
	p, err := c.Ask(context.Background(), "api_\x1b[31mkey\x1b[0m=\""+secret+"\" "+strings.Repeat("x", 9000), map[string]Question{"test": {Instructions: "Check api_key=\"" + secret + "\"", True: "yes", False: "no"}})
	if err != nil || p["test"] != .75 {
		t.Fatalf("answer %v, error %v", p, err)
	}
}

func TestClientErrorClassesAndSafeMessages(t *testing.T) {
	for _, tc := range []struct {
		status int
		class  ErrorClass
	}{
		{301, ClassServer}, {307, ClassServer}, {401, ClassAuth}, {402, ClassAuth}, {403, ClassAuth}, {404, ClassAuth},
		{429, ClassRate}, {400, ClassRejected}, {413, ClassRejected}, {422, ClassRejected}, {500, ClassServer}, {503, ClassServer},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "99999")
				w.WriteHeader(tc.status)
				io.WriteString(w, "response-canary-not-for-errors")
			}))
			defer s.Close()
			c := NewClient("fixture-key")
			c.Endpoint = s.URL
			_, err := c.Ask(context.Background(), "text", map[string]Question{"q": {Instructions: "question"}})
			me, ok := err.(*ModelError)
			if !ok || me.Class != tc.class || me.Status != tc.status {
				t.Fatalf("error %v", err)
			}
			if strings.Contains(err.Error(), "canary") || strings.Contains(err.Error(), s.URL) {
				t.Fatal("unsafe error")
			}
			if tc.status == 429 && me.RetryAfter != time.Hour {
				t.Fatalf("retry %s", me.RetryAfter)
			}
		})
	}
}

func TestClientMalformedAndOversizedResponses(t *testing.T) {
	for _, body := range []string{
		`not JSON response-canary`, `{}`, `{"answers":{"q":{}}}`, `{"answers":{"q":{"noul":null}}}`,
		`{"answers":{"q":{"noul":-0.01}}}`, `{"answers":{"q":{"noul":1.01}}}`,
		`{"answers":{"q":{"noul":"0.8"}}}`, `{"answers":{"q":{"type":"choice","noul":0.8}}}`,
		strings.Repeat("x", maxModelResponse+1),
	} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		c := NewClient("fixture-key")
		c.Endpoint = s.URL
		_, err := c.Ask(context.Background(), "text", map[string]Question{"q": {Instructions: "question"}})
		s.Close()
		if me, ok := err.(*ModelError); !ok || me.Class != ClassMalformed {
			t.Fatalf("error %v", err)
		}
		if strings.Contains(err.Error(), "canary") {
			t.Fatal("response leaked")
		}
	}
}

func TestClientNeverFollows307(t *testing.T) {
	var forwarded atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1) }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 307) }))
	defer redirect.Close()
	c := NewClient("fixture-key")
	c.Endpoint = redirect.URL
	_, err := c.Ask(context.Background(), "private fixture text", map[string]Question{"q": {Instructions: "question"}})
	if me, ok := err.(*ModelError); !ok || me.Class != ClassServer || forwarded.Load() != 0 {
		t.Fatalf("error %v; forwarded %d", err, forwarded.Load())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientEndpointPolicyAndTransportError(t *testing.T) {
	for _, endpoint := range []string{"http://example.invalid", "ftp://127.0.0.1", "https://user:pass@example.invalid", ":bad"} {
		c := NewClient("fixture-key")
		c.Endpoint = endpoint
		c.HTTPClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Error("refused endpoint contacted")
			return nil, errors.New("transport-canary")
		})
		_, err := c.Ask(context.Background(), "text", map[string]Question{"q": {Instructions: "question"}})
		if _, ok := err.(*ModelError); !ok {
			t.Fatalf("error %v", err)
		}
	}
	c := NewClient("fixture-key")
	c.HTTPClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 15*time.Second {
			t.Error("missing bounded request context")
		}
		return nil, errors.New("transport-canary")
	})
	_, err := c.Ask(context.Background(), "text", map[string]Question{"q": {Instructions: "question"}})
	if me, ok := err.(*ModelError); !ok || me.Class != ClassServer || strings.Contains(err.Error(), "canary") || strings.Contains(err.Error(), "https") {
		t.Fatalf("unsafe error %v", err)
	}
}

func TestClientContextStaysLiveThroughBody(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		cancel()
		<-r.Context().Done()
	}))
	defer s.Close()
	c := NewClient("fixture-key")
	c.Endpoint = s.URL
	_, err := c.Ask(ctx, "text", map[string]Question{"q": {Instructions: "question"}})
	if me, ok := err.(*ModelError); !ok || me.Class != ClassServer {
		t.Fatalf("error %v", err)
	}
	// A normal body read must not see cancellation merely because headers arrived.
	c.HTTPClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: &contextBody{ctx: r.Context(), Reader: strings.NewReader(`{"answers":{"q":{"noul":0.5}}}`)}}, nil
	})
	if _, err := c.Ask(context.Background(), "text", map[string]Question{"q": {Instructions: "question"}}); err != nil {
		t.Fatal(err)
	}
}

type contextBody struct {
	ctx context.Context
	*strings.Reader
}

func (b *contextBody) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	return b.Reader.Read(p)
}
func (*contextBody) Close() error { return nil }

func TestRetryAfterBounds(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for value, want := range map[string]time.Duration{"12": 12 * time.Second, "9999999999999999999999999999": time.Hour, "-1": 0, "invalid": 0,
		now.Add(5 * time.Minute).Format(http.TimeFormat): 5 * time.Minute, now.Add(2 * time.Hour).Format(http.TimeFormat): time.Hour, now.Add(-time.Hour).Format(http.TimeFormat): 0} {
		if got := retryAfter(value, now); got != want {
			t.Errorf("retry %q = %s, want %s", value, got, want)
		}
	}
}
