package longrun

import (
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/state"
)

// A takeover resumes the session only when aiq run would accept its id; an id
// a hook reported that the CLI would read as a flag starts fresh, and says why.
func TestResumableRefusesAnIDTheCLIWouldReadAsAFlag(t *testing.T) {
	for _, tt := range []struct {
		name   string
		l      state.Lease
		same   bool
		resume bool
		why    string
	}{
		{"plain id", state.Lease{SessionID: "5e551011-2b3c-4d5e-8f90-123456789abc", TurnStartedAt: 1}, true, true, ""},
		{"other provider", state.Lease{SessionID: "s1", TurnStartedAt: 1}, false, false, ""},
		{"no session", state.Lease{TurnStartedAt: 1}, true, false, ""},
		{"no completed turn", state.Lease{SessionID: "s1"}, true, false, ""},
		{"flag", state.Lease{SessionID: "--dangerously-bypass-approvals-and-sandbox", TurnStartedAt: 1}, true, false, "not one aiq can resume"},
		{"space", state.Lease{SessionID: "s1 --yolo", TurnStartedAt: 1}, true, false, "not one aiq can resume"},
	} {
		resume, why := resumable(tt.l, tt.same)
		if resume != tt.resume || (tt.why == "") != (why == "") || !strings.Contains(why, tt.why) {
			t.Errorf("%s: resume=%v why=%q", tt.name, resume, why)
		}
	}
}
