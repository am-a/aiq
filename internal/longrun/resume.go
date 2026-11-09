package longrun

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResumeTranscript checks the transcript a resumed long session names before
// its lease records it, and returns the path to record. The lease is what a
// rebuilt note quotes and what decides whether a move resumes, so the path must
// be a regular file whose name is the session's own: a slip that names another
// session's file would hand that session's words to a successor under this one's
// name. It is recorded with links resolved, so it survives an overlay rename.
func ResumeTranscript(provider, sessionID, path string) (string, error) {
	if path == "" {
		return "", errors.New("no transcript given")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s is not an absolute path", path)
	}
	path = filepath.Clean(path)
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	if sessionID == "" || !namesSession(provider, sessionID, path) {
		return "", fmt.Errorf("%s is not the transcript of session %s", path, sessionID)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return real, nil
}

// namesSession says whether path is where provider keeps session id's
// transcript: Claude's <id>.jsonl, Codex's rollout-<time>-<id>.jsonl and
// Antigravity's brain/<id>/.system_generated/logs/transcript.jsonl.
func namesSession(provider, id, path string) bool {
	if strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return false
	}
	base := filepath.Base(path)
	switch provider {
	case "claude":
		return base == id+".jsonl"
	case "codex":
		return strings.HasPrefix(base, "rollout-") && strings.HasSuffix(base, "-"+id+".jsonl")
	case "agy":
		return strings.HasSuffix(path, string(filepath.Separator)+filepath.Join("brain", id, ".system_generated", "logs", "transcript.jsonl"))
	}
	return false
}
