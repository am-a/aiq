// Package redact replaces values that match known secret formats with typed
// placeholders. Handoff notes use it, because a note quotes a session's
// prompts and tool output and can be read by another account or vendor.
//
// It matches formats only. A password in plain prose or a token in an unknown
// format passes through, so redacted text is not proof that no secret is left.
package redact

import (
	"regexp"
	"strings"
)

type rule struct {
	kind string
	re   *regexp.Regexp
	// wrap builds the replacement from the match's groups and the
	// placeholder; false leaves the match alone. nil means the placeholder
	// replaces the whole match.
	wrap func(sub []string, placeholder string) (string, bool)
}

// keepPrefix keeps group 1 in front of the placeholder.
func keepPrefix(sub []string, placeholder string) (string, bool) {
	return sub[1] + placeholder, true
}

// assignment keeps a key and its separator, unless the value reads as code: a
// variable, a call or a template names a secret without holding one.
func assignment(sub []string, placeholder string) (string, bool) {
	v := sub[3]
	if strings.ContainsAny(v[:1], "$<{[(%") || strings.Contains(v, "(") ||
		strings.Contains(v, "process.env") || strings.Contains(v, "os.environ") {
		return "", false
	}
	return sub[1] + sub[2] + placeholder, true
}

const (
	secretNames = `token|secret|password|passwd|pwd|credential|credentials|api[_-]?key|access[_-]?key|private[_-]?key|auth[_-]?key`
	pemBody     = `[A-Za-z0-9+/=\s\\:,.-]`
)

// The rules run in order. Specific formats come before the generic key=value
// rules, so a known token is named by its kind.
var rules = []rule{
	{kind: "private key", re: regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----(?:` + pemBody + `*?-----END [A-Z0-9 ]*PRIVATE KEY-----|` + pemBody + `+)`)},
	{kind: "orchestra invite", re: regexp.MustCompile(`\bor1\.[A-Za-z0-9_-]{16,}=*`)},
	{kind: "pairing invite", re: regexp.MustCompile(`\bap1\.[A-Za-z0-9_-]{16,}=*`)},
	{kind: "Anthropic key", re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	{kind: "OpenAI key", re: regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]{32,}`)},
	{kind: "GitHub token", re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`)},
	{kind: "AWS key id", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{kind: "Google key", re: regexp.MustCompile(`\b(?:AIza[0-9A-Za-z_-]{30,}|ya29\.[0-9A-Za-z_-]{20,})`)},
	{kind: "Slack token", re: regexp.MustCompile(`\bxox[abeoprs]-[0-9A-Za-z-]{10,}`)},
	{kind: "npm token", re: regexp.MustCompile(`\bnpm_[A-Za-z0-9]{30,}`)},
	{kind: "Stripe key", re: regexp.MustCompile(`\b[sr]k_(?:live|test)_[0-9A-Za-z]{16,}`)},
	{kind: "JWT", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{5,}`)},
	{kind: "URL credentials", re: regexp.MustCompile(`\b([a-zA-Z][a-zA-Z0-9+.-]*://)[^\s/:@'"\\]+:[^\s/@'"\\]+@`),
		wrap: func(sub []string, p string) (string, bool) { return sub[1] + p + "@", true }},
	{kind: "bearer token", re: regexp.MustCompile(`(?i)(\bbearer\s+)[A-Za-z0-9._~+/-]{16,}=*`), wrap: keepPrefix},
	{kind: "basic auth", re: regexp.MustCompile(`(?i)(\bauthorization\\*["']?\s*[:=]\s*\\*["']?basic\s+)[A-Za-z0-9+/]{8,}=*`), wrap: keepPrefix},
	{kind: "secret value", re: regexp.MustCompile(`(?i)([a-z0-9_.-]*(?:` + secretNames + `))(\\*["']?\s*(?::=|==?|:)\s*\\*["']?)([^\s"'\\,;]{8,})`), wrap: assignment},
	{kind: "secret value", re: regexp.MustCompile(`(?i)((?:^|[\s'"])--?[a-z0-9-]*(?:` + secretNames + `))(\s+\\*["']?)([^\s"'\\,;]{8,})`), wrap: assignment},
}

// Placeholder is what a value of this kind becomes.
func Placeholder(kind string) string { return "[redacted " + kind + "]" }

// Text returns s with every match replaced, and how many values of each kind
// it replaced. Running it again on its own result changes nothing.
func Text(s string) (string, map[string]int) {
	var hits map[string]int
	for _, r := range rules {
		placeholder := Placeholder(r.kind)
		s = r.re.ReplaceAllStringFunc(s, func(m string) string {
			out := placeholder
			if r.wrap != nil {
				sub := r.re.FindStringSubmatch(m)
				if sub == nil {
					return m
				}
				var ok bool
				if out, ok = r.wrap(sub, placeholder); !ok {
					return m
				}
			}
			if hits == nil {
				hits = map[string]int{}
			}
			hits[r.kind]++
			return out
		})
	}
	return s, hits
}
