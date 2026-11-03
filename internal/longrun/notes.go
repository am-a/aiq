package longrun

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/redact"
)

// ArchiveRetention is how long an archived handoff note is kept.
const ArchiveRetention = 30 * 24 * time.Hour

// archivedNote matches the names ArchiveNote gives; a workspace's current note
// and anything else in the directory never match.
var archivedNote = regexp.MustCompile(`^[0-9a-f]{16}\.(\d{8}-\d{6})\.md$`)

// lockNoteDir makes the handoff directory owner-only. A session may have
// created it under its own umask.
func lockNoteDir() {
	if err := os.MkdirAll(HandoffDir(), 0o700); err == nil {
		os.Chmod(HandoffDir(), 0o700)
	}
}

// PruneNotes deletes archived handoff notes past ArchiveRetention, counted from
// the archive time in the name.
func (s *Supervisor) PruneNotes(now time.Time) {
	lockNoteDir()
	removed, err := PruneArchives(now)
	if err != nil {
		s.Logf("pruning archived handoff notes: %v", err)
	}
	if removed > 0 {
		s.Logf("deleted %d archived handoff note(s) older than %d days", removed, int(ArchiveRetention.Hours()/24))
	}
}

// PruneArchives deletes the archived notes past ArchiveRetention and says how
// many went. It also deletes temporary files a crash left behind more than a day
// ago, without counting them. A workspace's current note stays until its next takeover.
func PruneArchives(now time.Time) (int, error) {
	entries, err := os.ReadDir(HandoffDir())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	var firstErr error
	for _, e := range entries {
		if strayTemp(e, now) {
			if err := os.Remove(filepath.Join(HandoffDir(), e.Name())); err != nil && firstErr == nil {
				firstErr = err
			}
			continue
		}
		m := archivedNote.FindStringSubmatch(e.Name())
		if m == nil || e.IsDir() { // a link set aside is removed, not followed
			continue
		}
		at, err := time.ParseInLocation("20060102-150405", m[1], time.Local)
		if err != nil || now.Sub(at) <= ArchiveRetention {
			continue
		}
		if err := os.Remove(filepath.Join(HandoffDir(), e.Name())); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}

// strayTemp reports whether e is a temporary note a crash left behind. Only the
// names writeNote and RedactNote use count, and only well after any write would end.
// That holds because a temporary file's modification time is when it was written:
// RedactNote restores the note's older time only after the rename.
func strayTemp(e os.DirEntry, now time.Time) bool {
	if !e.Type().IsRegular() || !(strings.HasPrefix(e.Name(), ".note-") || strings.HasPrefix(e.Name(), ".redact-")) {
		return false
	}
	fi, err := e.Info()
	return err == nil && now.Sub(fi.ModTime()) > 24*time.Hour
}

// writeNote creates the note at p whole, so a failed write leaves no partial note.
// A note already at p stays and the error matches os.ErrExist. Where hard links do
// not work, it falls back to a rename, which replaces any note there.
func writeNote(p, note string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".note-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.WriteString(note)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	err = os.Link(tmp.Name(), p)
	if err == nil || errors.Is(err, os.ErrExist) {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// RedactNote rewrites a note with known secret formats replaced, leaving its
// modification time as it was: takeovers read that time to tell an earlier
// note from a current one. It keeps no copy of the original, and makes the file
// owner-only even when nothing matched.
func RedactNote(p string) (map[string]int, error) {
	hits, out, fi, err := scanNote(p)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return nil, os.Chmod(p, 0o600)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".redact-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.WriteString(out)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), p)
	}
	if err != nil {
		return nil, err
	}
	// The secret is gone by now, so a failure here is not a failure to redact. It
	// comes after the rename because an old time before it would make the temporary
	// file look stray to PruneArchives.
	os.Chtimes(p, fi.ModTime(), fi.ModTime())
	return hits, nil
}

// ScanNote reports what RedactNote would replace in a note, changing nothing.
func ScanNote(p string) (map[string]int, error) {
	hits, _, _, err := scanNote(p)
	return hits, err
}

func scanNote(p string) (map[string]int, string, os.FileInfo, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, "", nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, "", nil, fmt.Errorf("%s is not a regular file", p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, "", nil, err
	}
	out, hits := redact.Text(string(b))
	return hits, out, fi, nil
}

// NoteFiles lists every note in the handoff directory, current and archived.
func NoteFiles() ([]string, error) {
	entries, err := os.ReadDir(HandoffDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".md") {
			files = append(files, filepath.Join(HandoffDir(), e.Name()))
		}
	}
	return files, nil
}

// LockNoteDir is lockNoteDir for the command line.
func LockNoteDir() { lockNoteDir() }

// DescribeHits names what was replaced, by kind and count, never by value.
func DescribeHits(hits map[string]int) string {
	parts := make([]string, 0, len(hits))
	for kind, n := range hits {
		parts = append(parts, fmt.Sprintf("%d %s", n, kind))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}
