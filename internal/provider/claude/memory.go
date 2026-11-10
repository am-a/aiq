package claude

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/orlenko/aiq/internal/paths"
)

// LaunchSettings renders one --settings JSON (Claude Code honours only the
// last --settings flag) carrying aiq's hooks when aiqBin is set and the
// auto-memory directory when memoryDir is set. Empty when neither is.
func LaunchSettings(aiqBin, memoryDir string) string {
	var parts []string
	if memoryDir != "" {
		parts = append(parts, `"autoMemoryDirectory":`+jsonString(memoryDir))
	}
	if aiqBin != "" {
		hook := func(event string) string {
			return `"` + event + `":[{"hooks":[{"type":"command","command":` + jsonString(aiqBin+" claude-hook "+strings.ToLower(event)) + `,"timeout":30}]}]`
		}
		parts = append(parts, `"hooks":{`+hook("SessionStart")+`,`+hook("UserPromptSubmit")+`,`+hook("Stop")+`}`)
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func jsonString(s string) string {
	enc, _ := json.Marshal(s)
	return string(enc)
}

// WithMemoryDir adds memoryDir to the --settings of a Claude command line
// that is not a long session's (longArgs carries it in aiq's own settings).
// Claude Code keeps only the last --settings, so when the arguments already
// have one (cmux's wrapper adds its hooks this way) the key goes into that
// value if it is an inline JSON object without the key; a settings file, or a
// value that already names a memory directory, is left as it is. The input
// slice is never modified.
func WithMemoryDir(args []string, memoryDir string) []string {
	if memoryDir == "" {
		return args
	}
	at, inline := -1, false // index of the last --settings value
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		switch {
		case a == "--settings" && i+1 < len(args):
			at, inline = i+1, false
			i++
		case a == "--settings":
			return args // no value; leave it to Claude Code to refuse
		case strings.HasPrefix(a, "--settings="):
			at, inline = i, true
		}
	}
	out := append([]string{}, args...)
	if at < 0 {
		return append([]string{"--settings", LaunchSettings("", memoryDir)}, out...)
	}
	value := out[at]
	if inline {
		value = strings.TrimPrefix(value, "--settings=")
	}
	merged, ok := addMemoryDir(value, memoryDir)
	if !ok {
		return args
	}
	if inline {
		merged = "--settings=" + merged
	}
	out[at] = merged
	return out
}

// addMemoryDir puts autoMemoryDirectory into an inline JSON settings object.
// It fails for anything else (a file path) and for an object that already
// has the key.
func addMemoryDir(value, memoryDir string) (string, bool) {
	v := strings.TrimSpace(value)
	if !strings.HasPrefix(v, "{") {
		return "", false
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal([]byte(v), &doc) != nil || doc == nil {
		return "", false
	}
	if _, ok := doc["autoMemoryDirectory"]; ok {
		return "", false
	}
	doc["autoMemoryDirectory"] = json.RawMessage(jsonString(memoryDir))
	out, err := json.Marshal(doc)
	if err != nil {
		return "", false
	}
	return string(out), true
}

// WorkingDir is the process's working directory as the kernel names it,
// which is what Claude Code keys projects on. os.Getwd prefers $PWD, which
// keeps the case the user typed on a case-insensitive file system.
func WorkingDir() string {
	dir, err := syscall.Getwd()
	if err != nil {
		return ""
	}
	return dir
}

// OverlayMemoryDir is the auto-memory directory an overlay launch in dir
// should use: the project's folder under the real ~/.claude/projects.
//
// Left alone, Claude Code puts memory under $CLAUDE_CONFIG_DIR/projects,
// which in an overlay is a symlink into ~/.claude. Every memory write then
// resolves into a .claude directory, and Claude Code's sensitive-path check
// asks for it before any allow rule is consulted, so no permission grant
// stops the prompt. Naming the real directory keeps the write on one
// spelling, which Claude Code's own memory carve-out allows.
//
// It must name the folder Claude Code would pick by itself, and no other:
// a session started in one directory must not get another project's
// memory. So it is empty (keep Claude Code's default) whenever aiq cannot
// be sure: a relative or unreadable dir, a symlinked .git, a non-ASCII path
// (Claude Code normalizes those first), a folder name long enough to get
// Claude Code's hash suffix, or a memory directory chosen anywhere else.
func OverlayMemoryDir(dir string) string {
	root := projectRoot(dir)
	if root == "" || memoryDirChosenElsewhere(dir, root) {
		return ""
	}
	slug := projectSlug(root)
	if slug == "" {
		return ""
	}
	return filepath.Join(paths.RealClaudeHome(), "projects", slug, "memory")
}

// memoryDirChosenElsewhere reports whether something other than aiq's flag
// may pick the memory directory: Claude Code's own environment overrides, or
// autoMemoryDirectory in the user's settings or in the project's (in dir or
// in its root). The flag would outrank the project's settings, so aiq leaves
// the choice alone. A settings file that cannot be read or parsed counts as
// choosing, since aiq cannot tell.
func memoryDirChosenElsewhere(dir, root string) bool {
	for _, env := range []string{"CLAUDE_CODE_REMOTE_MEMORY_DIR", "CLAUDE_COWORK_MEMORY_PATH_OVERRIDE"} {
		if os.Getenv(env) != "" {
			return true
		}
	}
	files := []string{filepath.Join(paths.RealClaudeHome(), "settings.json")}
	for _, d := range []string{dir, root} {
		files = append(files, filepath.Join(d, ".claude", "settings.json"), filepath.Join(d, ".claude", "settings.local.json"))
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			continue
		}
		if err != nil {
			return true
		}
		var doc map[string]json.RawMessage
		if json.Unmarshal(data, &doc) != nil {
			return true
		}
		if _, ok := doc["autoMemoryDirectory"]; ok {
			return true
		}
	}
	return false
}

// projectRoot is the directory Claude Code 2.1.322 keys project memory on,
// found the way it finds it: from the files, never by running git, so the
// environment, git's version and a repository's config cannot steer it.
//
// The nearest directory at or above dir holding a .git entry is the root.
// A .git directory makes it the root as it is (a commondir file in it is
// ignored). A .git file is followed only to a linked worktree's main
// checkout that checks out both ways (see worktreeMain). With no .git, dir
// itself is the root. Empty when dir is not absolute, a .git is a symlink,
// or a directory on the way cannot be read.
func projectRoot(dir string) string {
	if dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	start, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return ""
	}
	for d := start; ; {
		fi, err := os.Lstat(filepath.Join(d, ".git"))
		switch {
		case err == nil && fi.IsDir():
			return d
		case err == nil && fi.Mode().IsRegular():
			return worktreeMain(d)
		case err == nil:
			return "" // a symlink or something stranger
		case !errors.Is(err, fs.ErrNotExist):
			return ""
		}
		up := filepath.Dir(d)
		if up == d {
			return start
		}
		d = up
	}
}

// worktreeMain is the project root for root, whose .git is a file. It is the
// main checkout only when root is a linked worktree of it: .git names a
// gitdir inside <common>/worktrees, and that gitdir's own back-link names
// root/.git. A bare repository's worktrees key on the common directory, as
// Claude Code does. Anything else (a planted or broken .git file, a
// submodule) keys on root itself, which is also what Claude Code does.
func worktreeMain(root string) string {
	line, ok := readSmall(filepath.Join(root, ".git"))
	if !ok || !strings.HasPrefix(line, "gitdir:") {
		return root
	}
	gitdir := resolve(root, strings.TrimSpace(strings.TrimPrefix(line, "gitdir:")))
	common, ok := readSmall(filepath.Join(gitdir, "commondir"))
	if !ok {
		return root
	}
	commonDir := resolve(gitdir, common)
	if filepath.Dir(gitdir) != filepath.Join(commonDir, "worktrees") {
		return root
	}
	back, ok := readSmall(filepath.Join(gitdir, "gitdir"))
	if !ok {
		return root
	}
	if runtime.GOOS != "windows" && strings.Contains(root+gitdir+back+commonDir, `\`) {
		return root
	}
	backReal, err1 := filepath.EvalSymlinks(resolve(gitdir, back))
	rootReal, err2 := filepath.EvalSymlinks(root)
	if err1 != nil || err2 != nil || backReal != filepath.Join(rootReal, ".git") {
		return root
	}
	if filepath.Base(commonDir) != ".git" {
		if _, err := os.Lstat(filepath.Join(commonDir, ".git")); !errors.Is(err, fs.ErrNotExist) {
			return root
		}
		return commonDir
	}
	return filepath.Dir(commonDir)
}

func resolve(base, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}

// readSmall reads a short git pointer file, trimmed. It refuses a symlink,
// anything that is not a regular file, a file over 4 KiB, and a value with a
// NUL or a line break inside it.
func readSmall(path string) (string, bool) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 4096 {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 4096 {
		return "", false
	}
	s := strings.TrimSpace(string(data))
	if s == "" || strings.ContainsAny(s, "\x00\r\n") {
		return "", false
	}
	return s, true
}

// maxSlug is the longest folder name Claude Code uses unhashed.
const maxSlug = 200

// projectSlug names a project's folder under projects/ the way Claude Code
// does: every character that is not an ASCII letter or digit becomes "-".
// Empty for a path with any non-ASCII character (Claude Code normalizes
// those to NFC first, which aiq does not reproduce) and for a name past
// maxSlug (Claude Code adds a hash suffix aiq does not reproduce).
func projectSlug(root string) string {
	if root == "" || len(root) > maxSlug {
		return ""
	}
	b := []byte(root)
	for i, c := range b {
		switch {
		case c >= 0x80:
			return ""
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		default:
			b[i] = '-'
		}
	}
	return string(b)
}
