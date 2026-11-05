package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/provider/claude"
	"github.com/orlenko/aiq/internal/state"
)

func TestUnreadableLoginIsReportedNotExcluded(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	home := t.TempDir()
	cred := filepath.Join(home, ".credentials.json")
	if err := os.WriteFile(cred, []byte(`{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	acc := state.Account{ID: "claude/locked", Provider: "claude", Name: "locked", Enabled: true, Home: home, Identity: "l@example.com", Priority: 100}
	if err := st.AddAccount(acc); err != nil {
		t.Fatal(err)
	}
	st.SetCLILogin(acc.ID, true, "l@example.com", time.Now())
	p := &Pool{Cfg: config.Default(), St: st}

	if err := os.Chmod(cred, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(cred, 0o600)
	why := p.UnreadableLogin(acc, time.Now())
	if !strings.HasPrefix(why, "CLI login unreadable") || !strings.Contains(why, claude.CredentialStoreHint()) {
		t.Fatalf("reason %q", why)
	}
	u, _, _ := st.GetUsage(acc.ID)
	if u.CLILogin != state.LoginUnreadable || u.CLILoginError == "" || u.CLIIdentity != "l@example.com" {
		t.Fatalf("recorded %+v", u)
	}
	// Selection must not drop it silently; the launch decides.
	cands, err := p.Candidates("claude")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].UnavailableReason != "" {
		t.Fatalf("candidates %+v", cands)
	}
	v, _ := p.View(0)
	if v.Accounts[0].CLILoginProblem != why {
		t.Fatalf("view problem %q, want %q", v.Accounts[0].CLILoginProblem, why)
	}

	// Readable again: the launch check clears the recorded state.
	os.Chmod(cred, 0o600)
	if why := p.UnreadableLogin(acc, time.Now()); why != "" {
		t.Fatalf("readable credential reported %q", why)
	}
	if u, _, _ := st.GetUsage(acc.ID); u.CLILogin == state.LoginUnreadable || u.CLILoginError != "" {
		t.Fatalf("stale unreadable state %+v", u)
	}
}
