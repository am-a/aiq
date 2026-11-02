package redact

import (
	"strings"
	"testing"
)

// Every secret here is synthetic and assembled at run time, so the source
// holds nothing a secret scanner would report.
var (
	body   = strings.Repeat("Ab3_", 12)
	alnum  = strings.Repeat("Ab3d", 12)
	upper  = strings.Repeat("A1B2", 4)
	secret = "s3cr3t" + "-v4lue-" + "x9"
)

func TestTextReplacesKnownFormats(t *testing.T) {
	for _, c := range []struct{ kind, in, leak string }{
		{"orchestra invite", "join '" + "or1." + body + "' now", "or1." + body},
		{"pairing invite", "ap1." + body, body},
		{"Anthropic key", "sk-" + "ant-oat01-" + body, body},
		{"OpenAI key", "sk-" + "proj-" + body, body},
		{"GitHub token", "gh" + "p_" + alnum, alnum},
		{"GitHub token", "github_" + "pat_" + alnum, alnum},
		{"AWS key id", "AK" + "IA" + upper, upper},
		{"Google key", "AI" + "za" + body, body},
		{"Google key", "ya" + "29." + body, body},
		{"Slack token", "xo" + "xb-" + alnum, alnum},
		{"npm token", "np" + "m_" + alnum, alnum},
		{"Stripe key", "sk" + "_live_" + alnum, alnum},
		{"JWT", "ey" + "J" + alnum + "." + alnum + "." + alnum, alnum},
		{"URL credentials", "git clone https://me:" + secret + "@example.com/r.git", secret},
		{"bearer token", "curl -H 'Authorization: Bearer " + alnum + "'", alnum},
		{"basic auth", "Authorization: Basic " + alnum, alnum},
		{"secret value", "API_TOKEN=" + secret, secret},
		{"secret value", "password: " + secret, secret},
		{"secret value", `{"client_secret":"` + secret + `"}`, secret},
		{"secret value", `{\"cmd\":\"x\",\"password\":\"` + secret + `\"}`, secret},
		{"secret value", "tool --api-key " + secret + " --verbose", secret},
		{"private key", "-----BEGIN RSA PRIVATE" + " KEY-----\n" + alnum + "\n-----END RSA PRIVATE" + " KEY-----\nafter", alnum},
		{"private key", "-----BEGIN PRIVATE" + " KEY-----\\n" + alnum + "\\n", alnum},
	} {
		out, hits := Text(c.in)
		if strings.Contains(out, c.leak) {
			t.Errorf("%s: %q still holds the secret: %q", c.kind, c.in, out)
		}
		if !strings.Contains(out, Placeholder(c.kind)) || hits[c.kind] != 1 {
			t.Errorf("%s: %q gave %q, hits %v", c.kind, c.in, out, hits)
		}
		if again, more := Text(out); again != out || more != nil {
			t.Errorf("%s: a second pass changed %q to %q (%v)", c.kind, out, again, more)
		}
	}
}

// A quoted value is one value, whatever it holds.
func TestTextRedactsAWholeQuotedValue(t *testing.T) {
	ph := Placeholder("secret value")
	for in, want := range map[string]string{
		`{"password":"correct horse battery staple"}`:            `{"password":"` + ph + `"}`,
		`{"password":"abcdefgh,ijklmnop"}`:                       `{"password":"` + ph + `"}`,
		`{"password":"has;semi","user":"sam"}`:                   `{"password":"` + ph + `","user":"sam"}`,
		`{"password":"a\"b c"}`:                                  `{"password":"` + ph + `"}`,
		`{\"password\":\"correct horse\",\"a\":\"b\"}`:           `{\"password\":\"` + ph + `\",\"a\":\"b\"}`,
		`{\"password\":\"abc\\\"defghijk\"}`:                     `{\"password\":\"` + ph + `\"}`,
		`{\\\"password\\\":\\\"abc def\\\",\\\"a\\\":\\\"b\\\"}`: `{\\\"password\\\":\\\"` + ph + `\\\",\\\"a\\\":\\\"b\\\"}`,
		`{\\\"password\\\":\\\"abc\\\\\\\"def ghi\\\"}`:          `{\\\"password\\\":\\\"` + ph + `\\\"}`,
		`password = 'it\'s a secret'`:                            `password = '` + ph + `'`,
		`password = 'correct horse'`:                             `password = '` + ph + `'`,
		`password: "short"`:                                      `password: "` + ph + `"`,
		`tool --password "correct horse" --verbose`:              `tool --password "` + ph + `" --verbose`,
		`tool --password='correct horse' --verbose`:              `tool --password='` + ph + `' --verbose`,
		`PASSWORD=abcdefgh,ijklmnop next`:                        `PASSWORD=` + ph + ` next`,
		`TOKEN=abcdefghij; make`:                                 `TOKEN=` + ph + `; make`,
		"password: \"cut off before the quote\nnext line":        `password: "` + ph + "\nnext line",
	} {
		out, hits := Text(in)
		if out != want || hits["secret value"] != 1 {
			t.Errorf("%s\n got %s\nwant %s (%v)", in, out, want, hits)
		}
		if again, more := Text(out); again != out || more != nil {
			t.Errorf("a second pass changed %s to %s (%v)", out, again, more)
		}
	}
}

// One level of JSON encoding turns a quote with r backslashes into one with
// 2r+1. An even r closes the inner literal, whatever the total: a password
// ending in a backslash has a closing quote with more backslashes than its
// opening quote, and an odd r is a quote inside the value.
func TestTextTellsClosingQuotesByEncodingLevel(t *testing.T) {
	ph := Placeholder("secret value")
	// encode applies one level of JSON string encoding.
	encode := func(s string) string {
		s = strings.ReplaceAll(s, `\`, `\\`)
		return strings.ReplaceAll(s, `"`, `\"`)
	}
	for _, c := range []struct{ name, literal, inner string }{
		{"ends in a backslash", `correct horse\\`, `\\`},
		{"ends in two backslashes", `correct horse\\\\`, `\\\\`},
		{"holds a quote", `correct\" horse`, ""},
		{"holds a quote and ends in a backslash", `correct\" horse\\`, `\\`},
	} {
		for _, tail := range []string{`}`, `,"user":"sam"}`} {
			in := `{"password":"` + c.literal + `"` + tail
			want := `{"password":"` + ph + c.inner + `"` + tail
			for level := 1; level <= 4; level++ {
				in, want = encode(in), encode(want)
				// The field that holds the encoded text, as in tool arguments.
				doc, wantDoc := `{"body":"`+in+`"}`, `{"body":"`+want+`"}`
				out, hits := Text(doc)
				if out != wantDoc || hits["secret value"] != 1 {
					t.Errorf("%s, tail %s, level %d:\n got %s\nwant %s (%v)", c.name, tail, level, out, wantDoc, hits)
				}
				if again, more := Text(out); again != out || more != nil {
					t.Errorf("%s, tail %s, level %d: a second pass changed %s to %s (%v)", c.name, tail, level, out, again, more)
				}
			}
		}
	}
}

// A known token inside a longer password does not exempt the rest of it.
func TestTextRedactsTheRestOfAQuotedValueAfterAToken(t *testing.T) {
	in := `{"password":"` + "sk-" + "proj-" + body + ` private-suffix"}`
	out, _ := Text(in)
	if out != `{"password":"`+Placeholder("secret value")+`"}` {
		t.Errorf("got %s", out)
	}
	if again, more := Text(out); again != out || more != nil {
		t.Errorf("a second pass changed %s to %s (%v)", out, again, more)
	}
	// A value that is only a token keeps its kind.
	in = `{"password":"` + "sk-" + "proj-" + body + `"}`
	if out, _ = Text(in); out != `{"password":"`+Placeholder("OpenAI key")+`"}` {
		t.Errorf("got %s", out)
	}
}

// A literal cut off at a backslash is still redacted to the end of its line.
func TestTextRedactsALiteralCutOffAtABackslash(t *testing.T) {
	ph := Placeholder("secret value")
	for in, want := range map[string]string{
		`password: "abc def\`:     `password: "` + ph,
		`\"password\":\"abc def\`: `\"password\":\"` + ph,
	} {
		if out, _ := Text(in); out != want {
			t.Errorf("%s\n got %s\nwant %s", in, out, want)
		}
	}
}

func TestTextKeepsContextAroundASecret(t *testing.T) {
	out, _ := Text("run API_TOKEN=" + secret + " make deploy")
	if out != "run API_TOKEN="+Placeholder("secret value")+" make deploy" {
		t.Errorf("got %q", out)
	}
	out, _ = Text("https://me:" + secret + "@example.com/r.git")
	if out != "https://"+Placeholder("URL credentials")+"@example.com/r.git" {
		t.Errorf("got %q", out)
	}
	if out, _ = Text("-----BEGIN PRIVATE" + " KEY-----\n" + alnum + "\n-----END PRIVATE" + " KEY-----\nthe next line"); !strings.HasSuffix(out, "\nthe next line") {
		t.Errorf("text after the key was lost: %q", out)
	}
}

// What a successor needs to carry on stays readable.
func TestTextLeavesOrdinaryTextAlone(t *testing.T) {
	for _, in := range []string{
		"814efbe2c5a14f0e9b7d3a6c1f8e2d4b7a9c0e13",
		"session 183e5f9f-2128-4698-9bb8-494570023757",
		"m_a3ff25ed72f7a35bc04df3612bd8bbe5 mb_9d3aa5bcd065",
		"redact `or1.` invites from notes",
		"task-abcdefghijklmnopqrstuvwxyz0123456789abcd",
		"http://localhost:8080/a@b",
		"ssh://git@github.com/orlenko/aiq.git",
		"git@github.com:orlenko/aiq.git",
		"max_tokens: 128000000",
		"total_tokens=1234567890",
		"token_count: 123456789",
		"the password is short",
		"TOKEN=;;;;;;;;",
		"password: ,,,,,,,,,,",
		"a non-secret approach to rotation",
		"use anti-token measures",
		"TOKEN=$AIQ_DAEMON_TOKEN",
		"password: ${DB_PASSWORD}",
		`password: "${DB_PASSWORD}"`,
		`{"api_key":"<your key here>"}`,
		`if token == "" {`,
		"api_key: <your key here>",
		"token := tokenizer.Next()",
		"secret = os.environ['SOME_NAME']",
	} {
		if out, hits := Text(in); out != in || hits != nil {
			t.Errorf("%q became %q (%v)", in, out, hits)
		}
	}
}
