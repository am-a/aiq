// Package redact replaces values that match known secret formats with typed
// placeholders. Handoff notes use it, because a note quotes a session's
// prompts and tool output and can be read by another account or vendor.
//
// It matches formats only. A password in plain prose or a token in an unknown
// format passes through, so redacted text is not proof that no secret is left.
package redact

import (
	"regexp"
	"strconv"
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

// reference matches a value that names a secret without holding one: a shell
// or template variable, a <placeholder>, or text already redacted.
var reference = regexp.MustCompile(`^(?:\$\{?\w+\}?|\$\(.*\)|<[^<>]*>|\{\{.*\}\}|%\w+%|\[redacted [^\]]*\])$`)

// quoted keeps a key, its separator and the quotes around a whole literal. An
// empty literal or a reference stays as it is.
func quoted(sub []string, placeholder string) (string, bool) {
	if sub[3] == "" || reference.MatchString(sub[3]) {
		return "", false
	}
	return sub[1] + sub[2] + placeholder + sub[4], true
}

// bare is the same for an unquoted value, which ends at whitespace or a quote.
// A value that reads as code stays too: a call or an environment lookup names a
// secret. A comma or semicolon at the end is punctuation and stays outside.
func bare(sub []string, placeholder string) (string, bool) {
	v := strings.TrimRight(sub[3], ",;")
	if v == "" {
		return "", false
	}
	if reference.MatchString(v) || strings.ContainsAny(v[:1], "$<{[(%") || strings.Contains(v, "(") ||
		strings.Contains(v, "process.env") || strings.Contains(v, "os.environ") {
		return "", false
	}
	return sub[1] + sub[2] + placeholder + sub[3][len(v):], true
}

const (
	secretNames = `token|secret|password|passwd|pwd|[_.-]pass|credential|credentials|api[_-]?key|access[_-]?key|private[_-]?key|auth[_-]?key`
	pemBody     = `[A-Za-z0-9+/=\s\\:,.-]`

	// A key with its separator, and a flag; both end before the value.
	secretKey  = `([a-z0-9_.-]*(?:` + secretNames + `))(\\*["']?\s*(?::=|==?|:)\s*`
	secretFlag = `((?:^|[\s'"])--?[a-z0-9-]*(?:` + secretNames + `))(\s+`

	// Quoted literals run to their closing quote, or to the end of a line cut
	// off before one. Each form follows a key or a flag: the first group closes
	// the separator with its opening quote, the second is the value, the third
	// the closing quote.
	doubleLiteral = `")((?:[^"\\\n]|\\.|\\$)*)("|$)`
	singleLiteral = `')((?:[^'\\\n]|\\.|\\$)*)('|$)`
	bareValue     = `\\*["']?)([^\s"'\\]{8,})`
)

// escapedLiteral is a double-quoted literal inside JSON encoding, whose opening
// quote carries 1, 3, 7 or 15 backslashes. Encoding turns a quote with r
// backslashes into one with 2r+1, so the count tells a quote inside the value (r
// odd in the unencoded text) from a closing one (r even), even when a password
// ending in a backslash gives the closing quote more backslashes than the
// opening one. Any other run before a quote ends the literal, so that malformed
// text stops the match rather than preventing it.
func escapedLiteral(backslashes int) string {
	n := strconv.Itoa(backslashes)
	interior := strconv.Itoa(2*backslashes + 1)
	step := strconv.Itoa(2*backslashes + 2)
	return `\\{` + n + `}")((?:[^"\\\n]|\\+[^"\\\n]|\\{` + interior + `}(?:\\{` + step + `})*"|\\+$)*)(\\+"|$)`
}

// secretRules covers one way of naming a secret, a key or a flag.
func secretRules(name string) []rule {
	const kind = "secret value"
	compile := func(tail string) *regexp.Regexp { return regexp.MustCompile(`(?im)` + name + tail) }
	var out []rule
	for _, backslashes := range []int{15, 7, 3, 1} {
		out = append(out, rule{kind: kind, re: compile(escapedLiteral(backslashes)), wrap: quoted})
	}
	return append(out,
		rule{kind: kind, re: compile(doubleLiteral), wrap: quoted},
		rule{kind: kind, re: compile(singleLiteral), wrap: quoted},
		rule{kind: kind, re: compile(bareValue), wrap: bare},
	)
}

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
	{kind: "GitLab token", re: regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`)},
	{kind: "Hugging Face token", re: regexp.MustCompile(`\bhf_[A-Za-z0-9]{30,}`)},
	{kind: "DigitalOcean token", re: regexp.MustCompile(`\bdo[opr]_v1_[a-f0-9]{40,}`)},
	{kind: "SendGrid key", re: regexp.MustCompile(`\bSG\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}`)},
	{kind: "Tailscale key", re: regexp.MustCompile(`\btskey-[A-Za-z0-9-]{20,}`)},
	{kind: "PyPI token", re: regexp.MustCompile(`\bpypi-[A-Za-z0-9_-]{32,}`)},
	{kind: "age key", re: regexp.MustCompile(`\bAGE-SECRET-KEY-1[0-9A-Z]{40,}`)},
	{kind: "JWT", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{5,}`)},
	{kind: "URL credentials", re: regexp.MustCompile(`\b([a-zA-Z][a-zA-Z0-9+.-]*://)[^\s/:@'"\\]+:[^\s/@'"\\]+@`),
		wrap: func(sub []string, p string) (string, bool) { return sub[1] + p + "@", true }},
	{kind: "bearer token", re: regexp.MustCompile(`(?i)(\bbearer\s+)[A-Za-z0-9._~+/-]{16,}=*`), wrap: keepPrefix},
	{kind: "signed URL", re: regexp.MustCompile(`(?i)(\bX-Amz-(?:Signature|Security-Token)=)[^&\s"']+`), wrap: keepPrefix},
	{kind: "cookie", re: regexp.MustCompile(`(?i)(\b(?:set-)?cookie\\*["']?\s*:\s*\\*["']?)[^\s=;"']+=[^\n"']*`), wrap: keepPrefix},
	{kind: "basic auth", re: regexp.MustCompile(`(?i)(\bauthorization\\*["']?\s*[:=]\s*\\*["']?basic\s+)[A-Za-z0-9+/]{8,}=*`), wrap: keepPrefix},
}

func init() {
	rules = append(rules, secretRules(secretKey)...)
	rules = append(rules, secretRules(secretFlag)...)
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
