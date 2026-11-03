// Package daemon runs the machine-wide aiq service: it polls telemetry on a
// schedule, reaps dead leases, and serves a JSON API plus a small web page on
// the loopback interface. Shims never depend on it — the SQLite state is the
// contract — but they ping it after a limit event so the next poll is prompt.
package daemon

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/orlenko/aiq/internal/binpath"
	"github.com/orlenko/aiq/internal/cliupdate"
	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/longrun"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/proc"
)

//go:embed ui.html
var uiHTML []byte

//go:embed help.html
var helpHTML []byte

type Server struct {
	pool   *pool.Pool
	cfg    *config.Config
	log    *log.Logger
	pollCh chan []string

	mu       sync.Mutex
	lastPoll time.Time
	lastErr  string
	polling  bool
}

func New(cfg *config.Config, p *pool.Pool, logger *log.Logger) *Server {
	return &Server{
		pool:   p,
		cfg:    cfg,
		log:    logger,
		pollCh: make(chan []string, 8),
	}
}

// CheckListen reports whether addr is a numeric loopback address, the only
// kind the daemon serves on and aiq sends requests to. A name is refused because
// it can resolve to somewhere else, or to only one of the two loopback families.
func CheckListen(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("daemon.listen %q is not host:port: %w", addr, err)
	}
	if host != "127.0.0.1" && host != "::1" {
		return fmt.Errorf("daemon.listen %q must use 127.0.0.1 or [::1]", addr)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p <= 0 || p > 65535 || addr != net.JoinHostPort(host, strconv.Itoa(p)) {
		return fmt.Errorf("daemon.listen %q is not a loopback address with a plain port", addr)
	}
	return nil
}

// holdInstanceLock keeps a second daemon from running on this data directory. The
// port no longer does that alone, because a daemon with a refused address binds
// nothing. The lock lasts as long as the process.
func holdInstanceLock() error {
	f, err := os.OpenFile(lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("another aiq daemon is running on this data directory: %w", err)
		}
		return fmt.Errorf("locking %s: %w", lockPath(), err)
	}
	instanceLock = f // keeps the descriptor, and so the lock, open
	return nil
}

var instanceLock *os.File

func lockPath() string { return filepath.Join(paths.DataDir(), "daemon.lock") }

// LockHeld reports whether a running daemon holds the instance lock. A daemon from
// a version before the lock holds nothing, so false does not mean none is running.
// An error means the answer is unknown. The probe holds the lock for an instant,
// so a daemon starting at that moment exits and its service restarts it.
func LockHeld() (bool, error) {
	f, err := os.Open(lockPath())
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	return false, err // closing the file releases the probe's lock
}

// waitWithoutHTTP is what a daemon with a refused address does in place of serving.
// It is a variable so a test can check that Run reaches it.
var waitWithoutHTTP = func() { select {} }

// Run serves until the listener fails. With a listen address it must refuse, it
// serves nothing and leaves polling and session supervision running, so a bad
// setting for the status page does not strand long sessions. A listener failure
// on an accepted address still ends the process.
func (s *Server) Run() error {
	if err := holdInstanceLock(); err != nil {
		return err
	}
	listenErr := CheckListen(s.cfg.Daemon.Listen)
	if FixPATH() {
		s.log.Printf("PATH: moved %s to the front; a service installed with the shims buried mid-PATH cannot find any provider CLI", paths.ShimsDir())
	}
	go s.pollLoop()
	go s.housekeeping()
	if listenErr != nil {
		s.log.Printf("not serving the status page or API: %v", listenErr)
		waitWithoutHTTP()
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleUI)
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/help", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(helpHTML)
	})
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/poll", s.handlePoll)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	ln, err := net.Listen("tcp", s.cfg.Daemon.Listen)
	if err != nil {
		return err
	}
	if a, ok := ln.Addr().(*net.TCPAddr); !ok || !a.IP.IsLoopback() {
		ln.Close()
		return fmt.Errorf("daemon.listen %q bound to %v, which is not loopback", s.cfg.Daemon.Listen, ln.Addr())
	}
	s.log.Printf("aiq daemon listening on http://%s/", s.cfg.Daemon.Listen)
	srv := &http.Server{Handler: localOnly(mux), ReadHeaderTimeout: 5 * time.Second}
	return srv.Serve(ln)
}

func (s *Server) pollLoop() {
	interval := time.Duration(s.cfg.Poll.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	s.poll(nil)
	updater := s.updater()
	if updater != nil {
		updater.Tick(time.Now())
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.poll(nil)
			// Between polls, on the poll goroutine: the daemon's own
			// scheduled polls never start a CLI mid-reinstall.
			if updater != nil {
				updater.Tick(time.Now())
			}
		case ids := <-s.pollCh:
			// Coalesce a burst of requests.
			time.Sleep(2 * time.Second)
			for len(s.pollCh) > 0 {
				more := <-s.pollCh
				if len(more) == 0 {
					ids = nil
				} else if ids != nil {
					ids = append(ids, more...)
				}
			}
			s.poll(ids)
		}
	}
}

// updater keeps npm-installed CLIs current; nil when updates are off.
func (s *Server) updater() *cliupdate.Updater {
	if !s.cfg.Updates.Enabled {
		return nil
	}
	return &cliupdate.Updater{
		Resolve: func(provider string) (string, error) {
			return binpath.Resolve(provider, s.cfg.Provider(provider).Binary, paths.ShimsDir(), nil)
		},
		Interval: time.Duration(s.cfg.Updates.IntervalHours * float64(time.Hour)),
		Logf:     s.log.Printf,
		Event: func(provider, detail string) {
			s.pool.St.LogEvent(provider, "", "update", detail, time.Now())
		},
	}
}

func (s *Server) poll(ids []string) {
	s.mu.Lock()
	if s.polling {
		s.mu.Unlock()
		return
	}
	s.polling = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.polling = false
		s.mu.Unlock()
	}()

	results := s.pool.Refresh(ids...)
	var ok, failed []string
	for _, r := range results {
		if r.Err != nil {
			failed = append(failed, r.ID+": "+r.Err.Error())
		} else {
			ok = append(ok, r.ID)
		}
	}
	s.mu.Lock()
	s.lastPoll = time.Now()
	s.lastErr = strings.Join(failed, "; ")
	s.mu.Unlock()
	if len(ok) > 0 {
		s.log.Printf("poll: updated %s", strings.Join(ok, ", "))
	}
	for _, f := range failed {
		s.log.Printf("poll: %s", f)
	}
}

func (s *Server) housekeeping() {
	self, _ := os.Executable()
	sup := &longrun.Supervisor{Pool: s.pool, Logf: s.log.Printf, AiqBin: self,
		Refresh: func(id string) { s.poll([]string{id}) }}
	interval := time.Duration(s.cfg.Long.CheckIntervalSeconds) * time.Second
	for {
		time.Sleep(interval)
		s.pool.St.PruneLeases(pool.Hostname(), proc.Alive)
		s.pool.St.TrimEvents(2000)
		s.pool.St.TrimLaunches(50000)
		sup.PruneNotes(time.Now())
		sup.Tick()
	}
}

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(uiHTML)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	view, err := s.pool.View(50)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.mu.Lock()
	extra := map[string]any{
		"last_poll":     unixOrZero(s.lastPoll),
		"last_error":    s.lastErr,
		"polling":       s.polling,
		"poll_interval": s.cfg.Poll.IntervalSeconds,
		"listen":        s.cfg.Daemon.Listen,
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"daemon": extra, "view": view})
}

func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var ids []string
	if v := r.URL.Query().Get("account"); v != "" {
		ids = strings.Split(v, ",")
	}
	select {
	case s.pollCh <- ids:
	default:
	}
	w.Write([]byte("queued\n"))
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.pool.St.ListEvents(200)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(events)
}

// localOnly rejects a request from a non-loopback peer, one whose Host is not a
// loopback name (a page that rebinds a DNS name to 127.0.0.1), and one whose
// Origin is not this same host (any other website, or Origin: null). A browser
// sends Origin on every cross-site POST. Requests with no Origin, such as aiq's own
// and curl's, pass. It cannot see the real client behind a local port forward.
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, _, err := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(peer); err != nil || ip == nil || !ip.IsLoopback() {
			http.Error(w, "forbidden peer", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				http.Error(w, "forbidden site", http.StatusForbidden)
				return
			}
		}
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		if host != "localhost" && host != "127.0.0.1" && host != "::1" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Notify asks a running daemon to poll soon, and says whether one took the request.
// It never blocks a launch: one second timeout, errors ignored.
func Notify(listen string, accountIDs ...string) bool {
	if CheckListen(listen) != nil {
		return false
	}
	url := "http://" + listen + "/api/poll"
	if len(accountIDs) > 0 {
		url += "?account=" + strings.Join(accountIDs, ",")
	}
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Post(url, "text/plain", nil)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 300
}

// Alive reports whether a daemon answers on listen.
func Alive(listen string) bool {
	if CheckListen(listen) != nil {
		return false
	}
	client := &http.Client{Timeout: 700 * time.Millisecond}
	resp, err := client.Get("http://" + listen + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// LogPath is where the service writes its log.
func LogPath() string { return filepath.Join(paths.LogDir(), "daemon.log") }

// OpenLog opens (creating) the daemon log for appending.
func OpenLog() (*os.File, error) {
	if err := os.MkdirAll(paths.LogDir(), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(LogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
