package daemon

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/config"
)

func TestCheckListen(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:7379", "[::1]:7379"} {
		if err := CheckListen(addr); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
	for _, addr := range []string{"", "localhost:7379", "0.0.0.0:7379", "[::]:7379", "192.0.2.5:7379", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:x", "127.0.0.1:70000", "example.com:7379", "127.0.0.1:+7379", "[127.0.0.1]:7379", "127.0.0.1:07379"} {
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
		name, peer, host, origin, method, site string
		want                                   int
	}{
		{"aiq's own request", "127.0.0.1:50000", host, "", "POST", "", 204},
		{"the status page", "127.0.0.1:50000", host, "http://" + host, "POST", "", 204},
		{"ipv6 loopback peer", "[::1]:50000", "[::1]:7379", "http://[::1]:7379", "GET", "", 204},
		{"a forwarded local port", "127.0.0.1:50000", "127.0.0.1:8000", "http://127.0.0.1:8000", "GET", "", 204},
		{"a website's POST", "127.0.0.1:50000", host, "https://example.com", "POST", "", 403},
		{"a sandboxed frame", "127.0.0.1:50000", host, "null", "POST", "", 403},
		{"another local port's page", "127.0.0.1:50000", host, "http://127.0.0.1:3000", "POST", "", 403},
		{"the same host over https", "127.0.0.1:50000", host, "https://" + host, "POST", "", 403},
		{"a rebound name", "127.0.0.1:50000", "evil.example:7379", "", "GET", "", 403},
		{"a rebound name with its own origin", "127.0.0.1:50000", "evil.example:7379", "http://evil.example:7379", "GET", "", 403},
		{"a network peer", "192.0.2.5:50000", host, "", "GET", "", 403},
		{"a network peer naming localhost", "192.0.2.5:50000", "localhost:7379", "", "GET", "", 403},
		{"an unparseable peer", "somewhere", host, "", "GET", "", 403},
		{"a cross-site POST with no Origin", "127.0.0.1:50000", host, "", "POST", "cross-site", 403},
		{"a same-site POST with no Origin", "127.0.0.1:50000", host, "", "POST", "same-site", 403},
		{"the page's own POST", "127.0.0.1:50000", host, "http://" + host, "POST", "same-origin", 204},
		{"a typed address", "127.0.0.1:50000", host, "", "GET", "none", 204},
		{"a framed page load", "127.0.0.1:50000", host, "", "GET", "cross-site", 204},
	} {
		r := httptest.NewRequest(c.method, "/api/poll", nil)
		r.RemoteAddr, r.Host = c.peer, c.host
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.site != "" {
			r.Header.Set("Sec-Fetch-Site", c.site)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, w.Code, c.want)
		}
		if c.want == 204 && (w.Header().Get("Cross-Origin-Resource-Policy") != "same-origin" || w.Header().Get("X-Frame-Options") != "DENY") {
			t.Errorf("%s: missing security headers", c.name)
		}
	}
}

// A refused address must leave the daemon running under its lock, with polling and
// supervision started: returning the error would make the service restart it forever.
func TestRunWithARefusedAddressKeepsRunningWithoutHTTP(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("AIQ_DATA_DIR", root)
	t.Setenv("PATH", os.Getenv("PATH")) // Run may reorder PATH
	cfg := config.Default()
	closed := httptest.NewServer(http.NotFoundHandler())
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(closed.URL, "http://"))
	closed.Close()
	cfg.Daemon.Listen = net.JoinHostPort("0.0.0.0", port)
	s := New(cfg, nil, log.New(io.Discard, "", 0))

	lockedAt := func(step string) {
		t.Helper()
		if held, err := LockHeld(); err != nil || !held {
			t.Errorf("no lock held at %s: held %v, err %v", step, held, err)
		}
	}
	defer func(loops func(*Server), wait func()) { startLoops, waitWithoutHTTP = loops, wait }(startLoops, waitWithoutHTTP)
	started, waited := false, false
	startLoops = func(*Server) { started = true; lockedAt("the start of the loops") }
	waitWithoutHTTP = func() { waited = true; lockedAt("the wait") }
	defer func() {
		if instanceLock != nil {
			instanceLock.Close()
			instanceLock = nil
		}
	}()
	if err := s.Run(); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	if !started || !waited {
		t.Fatalf("Run did not reach the mode with loops and no HTTP: started %v, waited %v", started, waited)
	}
}

// Two daemons would both supervise the same sessions, so a daemon that finds an
// older one, which holds no lock, starts nothing.
func TestRunStopsWhenAnOlderDaemonAnswers(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("AIQ_DATA_DIR", root)
	t.Setenv("PATH", os.Getenv("PATH"))
	older := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte("ok\n"))
		}
	}))
	defer older.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(older.URL, "http://"))
	cfg := config.Default()
	defer func() {
		if instanceLock != nil {
			instanceLock.Close()
			instanceLock = nil
		}
	}()
	defer func(loops func(*Server)) { startLoops = loops }(startLoops)
	startLoops = func(*Server) { t.Error("the loops started beside an older daemon") }
	for _, listen := range []string{net.JoinHostPort("0.0.0.0", port), net.JoinHostPort("127.0.0.1", port)} {
		cfg.Daemon.Listen = listen
		if err := New(cfg, nil, log.New(io.Discard, "", 0)).Run(); err == nil || !strings.Contains(err.Error(), "older") {
			t.Errorf("%s: Run returned %v", listen, err)
		}
		if instanceLock != nil {
			instanceLock.Close()
			instanceLock = nil
		}
	}
}

func TestLockHeldReportsAnUnreadableLock(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read the lock")
	}
	data := t.TempDir()
	t.Setenv("AIQ_DATA_DIR", data)
	if err := os.WriteFile(filepath.Join(data, "daemon.lock"), nil, 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := LockHeld(); err == nil {
		t.Fatal("no error for a lock that cannot be read")
	}
}

func TestLockHeldWithNoLockFile(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	if held, err := LockHeld(); held || err != nil {
		t.Fatalf("held %v, err %v", held, err)
	}
	if err := holdInstanceLock(); err != nil {
		t.Fatal(err)
	}
	if held, err := LockHeld(); !held || err != nil {
		t.Fatalf("after locking: held %v, err %v", held, err)
	}
	instanceLock.Close()
	instanceLock = nil
	if held, err := LockHeld(); held || err != nil {
		t.Fatalf("after release: held %v, err %v", held, err)
	}
}

func TestInstanceLockRefusesASecondDaemon(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	defer func() {
		if instanceLock != nil {
			instanceLock.Close()
			instanceLock = nil
		}
	}()
	if err := holdInstanceLock(); err != nil {
		t.Fatal(err)
	}
	held := instanceLock
	if holdInstanceLock() == nil {
		t.Fatal("a second daemon took the lock")
	}
	if instanceLock != held {
		t.Fatal("the failed attempt replaced the held lock")
	}
}
