package longrun

import (
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/state"
)

// A takeover resumes the session only when aiq run would accept its id; an id
// a hook reported that the CLI would read as a flag starts fresh, and says why.
func TestCarriesOverRefusesAnIDTheCLIWouldReadAsAFlag(t *testing.T) {
	s := &Supervisor{}
	acc := state.Account{Provider: "codex"}
	for _, tt := range []struct {
		name   string
		id     string
		resume bool
	}{
		{"plain id", "5e551011-2b3c-4d5e-8f90-123456789abc", true},
		{"flag", "--dangerously-bypass-approvals-and-sandbox", false},
		{"space", "s1 --yolo", false},
	} {
		l := state.Lease{Provider: "codex", SessionID: tt.id, TurnStartedAt: 1}
		if got := s.carriesOver(l, acc); got != tt.resume {
			t.Errorf("%s: carriesOver = %v", tt.name, got)
		}
		if why := badSessionID(l); (why == "") != tt.resume || !tt.resume && !strings.Contains(why, "not one aiq can resume") {
			t.Errorf("%s: badSessionID = %q", tt.name, why)
		}
	}
	if why := badSessionID(state.Lease{}); why != "" {
		t.Errorf("no session: %q", why)
	}
}
