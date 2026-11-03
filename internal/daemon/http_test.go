package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCheckListen(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:7379", "[::1]:7379"} {
		if err := CheckListen(addr); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
	for _, addr := range []string{"", "localhost:7379", "0.0.0.0:7379", "[::]:7379", "192.0.2.5:7379", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:x", "127.0.0.1:70000", "example.com:7379"} {
		if CheckListen(addr) == nil {
			t.Errorf("%q was accepted", addr)
		}
	}
}

func TestClientsDoNotContactARefusedAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a request reached %s", r.URL)
	}))
	defer srv.Close()
	bad := "localhost:" + srv.URL[len("http://127.0.0.1:"):]
	Notify(bad, "a/b")
	if Alive(bad) {
		t.Error("a refused address counted as alive")
	}
}

func TestLocalOnly(t *testing.T) {
	const host = "127.0.0.1:7379"
	h := localOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, c := range []struct {
		name, peer, host, origin, method string
		want                             int
	}{
		{"aiq's own request", "127.0.0.1:50000", host, "", "POST", 204},
		{"the status page", "127.0.0.1:50000", host, "http://" + host, "POST", 204},
		{"ipv6 loopback peer", "[::1]:50000", "[::1]:7379", "http://[::1]:7379", "GET", 204},
		{"a forwarded local port", "127.0.0.1:50000", "127.0.0.1:8000", "http://127.0.0.1:8000", "GET", 204},
		{"a website's POST", "127.0.0.1:50000", host, "https://example.com", "POST", 403},
		{"a sandboxed frame", "127.0.0.1:50000", host, "null", "POST", 403},
		{"another local port's page", "127.0.0.1:50000", host, "http://127.0.0.1:3000", "POST", 403},
		{"the same host over https", "127.0.0.1:50000", host, "https://" + host, "POST", 403},
		{"a rebound name", "127.0.0.1:50000", "evil.example:7379", "", "GET", 403},
		{"a rebound name with its own origin", "127.0.0.1:50000", "evil.example:7379", "http://evil.example:7379", "GET", 403},
		{"a network peer", "192.0.2.5:50000", host, "", "GET", 403},
		{"a network peer naming localhost", "192.0.2.5:50000", "localhost:7379", "", "GET", 403},
		{"an unparseable peer", "somewhere", host, "", "GET", 403},
	} {
		r := httptest.NewRequest(c.method, "/api/poll", nil)
		r.RemoteAddr, r.Host = c.peer, c.host
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, w.Code, c.want)
		}
		if c.want == 204 && w.Header().Get("Cross-Origin-Resource-Policy") != "same-origin" {
			t.Errorf("%s: no resource policy", c.name)
		}
	}
}

func TestPollRequestsAreLimited(t *testing.T) {
	s := &Server{pollCh: make(chan []string, 8)}
	now := time.Unix(1000, 0)
	if !s.allowPollRequest(now) {
		t.Fatal("the first request was dropped")
	}
	if s.allowPollRequest(now.Add(pollRequestGap - time.Millisecond)) {
		t.Error("a request inside the gap was allowed")
	}
	if !s.allowPollRequest(now.Add(pollRequestGap)) {
		t.Error("a request after the gap was dropped")
	}
	post := func() {
		w := httptest.NewRecorder()
		s.handlePoll(w, httptest.NewRequest("POST", "/api/poll?account=a/b", nil))
		if w.Code != 200 {
			t.Errorf("status %d", w.Code)
		}
	}
	s.lastPollRequest = time.Time{}
	post()
	post()
	if len(s.pollCh) != 1 {
		t.Errorf("%d polls queued, want 1", len(s.pollCh))
	}
}
