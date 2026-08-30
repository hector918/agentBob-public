package scrub

import (
	"encoding/base64"
	"strings"
	"testing"
)

const marker = redactionMarker

// makeB64 returns a base64-alphabet run of exactly n characters. Uses a
// repeating pattern over [A-Za-z0-9] so it never accidentally contains the
// OpenSSH armor marker substring.
func makeB64(n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(alphabet[i%len(alphabet)])
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Per-pattern: positive (secret redacted) + negative (lookalike preserved).
// ---------------------------------------------------------------------------

func TestPatterns(t *testing.T) {
	longSecret := makeB64(40) // >= 30 for awsSecret body

	cases := []struct {
		name string
		in   string
		// wantContains: substrings that must be present in the output.
		wantContains []string
		// wantAbsent: substrings that must NOT be present in the output.
		wantAbsent []string
		// wantEqual: if non-empty, output must equal this exactly (used for
		// negative/lookalike cases that should pass through unchanged).
		wantEqual string
	}{
		// -- pemBlock --------------------------------------------------------
		{
			name:         "pem/positive-rsa-private-key",
			in:           "before\n-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\nsecretbytes\n-----END RSA PRIVATE KEY-----\nafter",
			wantContains: []string{"before", marker, "after"},
			wantAbsent:   []string{"MIIEowIBAAKCAQEA", "secretbytes", "BEGIN RSA"},
		},
		{
			// Regression guard for the fix: pemBlock used `[A-Z ]+`
			// (>=1 char) before the literal KEY/CERTIFICATE word, so a bare
			// RFC 7468 `-----BEGIN CERTIFICATE-----` block (the most common
			// cert form, no word before "CERTIFICATE") slipped through
			// un-redacted. Quantifier is now `[A-Z ]*` — the bare cert must
			// be redacted.
			name:         "pem/positive-certificate-bare-cert-redacted",
			in:           "-----BEGIN CERTIFICATE-----\nMIIC...\n-----END CERTIFICATE-----",
			wantContains: []string{marker},
			wantAbsent:   []string{"MIIC", "BEGIN CERTIFICATE"},
		},
		{
			// Control: a multi-word cert label ("TRUSTED CERTIFICATE") DOES
			// match because "TRUSTED " supplies the required `[A-Z ]+`. This
			// pins down that the gap is specifically the single-word label.
			name:         "pem/positive-cert-multiword-label",
			in:           "-----BEGIN TRUSTED CERTIFICATE-----\nMIIC...\n-----END TRUSTED CERTIFICATE-----",
			wantContains: []string{marker},
			wantAbsent:   []string{"MIIC"},
		},
		{
			// Lookalike: prose that mentions PEM-ish words but is not a real
			// BEGIN..KEY/CERTIFICATE..END block.
			name:      "pem/negative-prose-mentioning-keys",
			in:        "Please generate a private key and a certificate for me, thanks.",
			wantEqual: "Please generate a private key and a certificate for me, thanks.",
		},
		{
			// Lookalike: BEGIN marker but the armor word is neither KEY nor
			// CERTIFICATE, so the regex must not match.
			name:      "pem/negative-begin-message-not-key",
			in:        "-----BEGIN PGP MESSAGE-----\nhQEMA\n-----END PGP MESSAGE-----",
			wantEqual: "-----BEGIN PGP MESSAGE-----\nhQEMA\n-----END PGP MESSAGE-----",
		},
		{
			// Regression guard (audit, B37): a PEM block whose END
			// marker was cut off (upstream byte caps HEAD-truncate exec output
			// BEFORE scrub runs) used to pass through byte-for-byte — pemBlock
			// requires a matching END and the 64-70 char wrapped lines never
			// hit the 200-char base64 net. The orphan-BEGIN fallback must
			// redact from the surviving header to the end of the string.
			name:         "pem/positive-orphan-begin-truncated-key",
			in:           "log tail:\n-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQ\ncutoffmidbody",
			wantContains: []string{"log tail:", marker},
			wantAbsent:   []string{"BEGIN OPENSSH", "b3BlbnNzaC1rZXktdjEAAAAA", "cutoffmidbody"},
		},

		// -- opensshArmorBase64 ---------------------------------------------
		{
			// The literal base64 prefix that decodes to the OpenSSH armor
			// header, as seen JSON-escaped / mangled in transit.
			name:         "openssh-armor/positive",
			in:           "leaked: LS0tLS1CRUdJTiBPUEVOU1NIIFBSSVZBVEUgS0VZLS0tLS0aGVsbG8 end",
			wantContains: []string{"leaked: ", marker, " end"},
			wantAbsent:   []string{"LS0tLS1CRUdJTiBPUEVOU1NIIFBSSVZBVEUgS0VZLS0tLS0"},
		},
		{
			// Lookalike: a shorter / unrelated base64 token that does NOT
			// contain the full armor prefix.
			name:      "openssh-armor/negative-unrelated-b64",
			in:        "token LS0tLS1CRUdJTiBQVUJMSUM端 done",
			wantEqual: "token LS0tLS1CRUdJTiBQVUJMSUM端 done",
		},

		// -- awsSecret -------------------------------------------------------
		{
			name:         "aws/positive-export",
			in:           "export AWS_SECRET_ACCESS_KEY=" + longSecret + "\nnext line",
			wantContains: []string{"export ", marker, "next line"},
			wantAbsent:   []string{longSecret},
		},
		{
			name:         "aws/positive-quoted-colon",
			in:           `AWS_SECRET_ACCESS_KEY: "` + longSecret + `"`,
			wantContains: []string{marker},
			wantAbsent:   []string{longSecret},
		},
		{
			// Lookalike: the key name is present but the value is too short
			// (< 30 chars) to be a real secret.
			name:      "aws/negative-short-value",
			in:        "AWS_SECRET_ACCESS_KEY=short",
			wantEqual: "AWS_SECRET_ACCESS_KEY=short",
		},

		// -- slackBot --------------------------------------------------------
		{
			name:         "slack/positive",
			in:           "use xoxb-1234567890-ABCDEFGHIJ in the bot config",
			wantContains: []string{"use ", marker, " in the bot config"},
			wantAbsent:   []string{"xoxb-1234567890-ABCDEFGHIJ"},
		},
		{
			// Lookalike: prefix present but body too short (< 10 chars).
			name:      "slack/negative-short",
			in:        "xoxb-short",
			wantEqual: "xoxb-short",
		},

		// -- githubPAT -------------------------------------------------------
		{
			name:         "github/positive-ghp",
			in:           "token ghp_" + strings.Repeat("a", 36) + " here",
			wantContains: []string{"token ", marker, " here"},
			wantAbsent:   []string{"ghp_" + strings.Repeat("a", 36)},
		},
		{
			name:         "github/positive-fine-grained",
			in:           "github_pat_" + strings.Repeat("B", 30),
			wantContains: []string{marker},
			wantAbsent:   []string{"github_pat_" + strings.Repeat("B", 30)},
		},
		{
			// Lookalike: ghp_ prefix but body too short (< 20 chars).
			name:      "github/negative-short",
			in:        "ghp_tooshort",
			wantEqual: "ghp_tooshort",
		},

		// -- dsnUserinfoPassword --------------------------------------------
		{
			// pg DSN: redact ONLY the password, keep scheme/user/host/db so
			// the log stays diagnosable.
			name:         "dsn/positive-postgres",
			in:           "pq: connect failed: postgres://bob:s3cr3tP4ss@db.host:5432/agent173",
			wantContains: []string{"postgres://bob:", marker, "@db.host:5432/agent173"},
			wantAbsent:   []string{"s3cr3tP4ss"},
		},
		{
			name:         "dsn/positive-redis",
			in:           "redis://user:hunter2hunter2@127.0.0.1:6379/0",
			wantContains: []string{"redis://user:", marker, "@127.0.0.1:6379/0"},
			wantAbsent:   []string{"hunter2hunter2"},
		},
		{
			// Lookalike: a URL with NO userinfo password must pass through.
			name:      "dsn/negative-no-userinfo",
			in:        "GET https://api.example.com/v1/items?id=42",
			wantEqual: "GET https://api.example.com/v1/items?id=42",
		},

		// -- anthropic / openai / xai keys ----------------------------------
		{
			name:         "key/positive-anthropic",
			in:           "ANTHROPIC_API_KEY uses sk-ant-api03-" + strings.Repeat("A", 30) + " today",
			wantContains: []string{marker},
			wantAbsent:   []string{"sk-ant-api03-" + strings.Repeat("A", 30)},
		},
		{
			name:         "key/positive-openai",
			in:           "key sk-" + strings.Repeat("B", 32) + " end",
			wantContains: []string{"key ", marker, " end"},
			wantAbsent:   []string{"sk-" + strings.Repeat("B", 32)},
		},
		{
			name:         "key/positive-xai",
			in:           "xai-" + strings.Repeat("C", 40),
			wantContains: []string{marker},
			wantAbsent:   []string{"xai-" + strings.Repeat("C", 40)},
		},
		{
			// Lookalike: a short `sk-` token in prose (< 20 char body).
			name:      "key/negative-short-sk",
			in:        "the sku is sk-12345 ok",
			wantEqual: "the sku is sk-12345 ok",
		},
		{
			// Modern OpenAI project key: body contains -/_ so the bare
			// alnum-only sk- pattern never matches it.
			name:         "key/positive-openai-proj",
			in:           "key sk-proj-Ab3dE_fGh-" + strings.Repeat("D", 24) + " end",
			wantContains: []string{"key ", marker, " end"},
			wantAbsent:   []string{"sk-proj-Ab3dE_fGh-"},
		},
		{
			name:         "key/positive-openai-svcacct",
			in:           "sk-svcacct-" + strings.Repeat("E", 12) + "_" + strings.Repeat("F", 12),
			wantContains: []string{marker},
			wantAbsent:   []string{"sk-svcacct-"},
		},
		{
			// Lookalike: short body after the proj prefix stays untouched.
			name:      "key/negative-short-proj",
			in:        "see sk-proj-abc for details",
			wantEqual: "see sk-proj-abc for details",
		},
		{
			// Regression guard (audit, B38): the bare `sk-` pattern
			// had no left boundary, so hyphenated IDs ending in "sk" (task-,
			// disk-, risk-) + a 20-alnum run were corrupted mid-word
			// ("ta[REDACTED:credential] is running"). Must pass through intact.
			name:      "key/negative-embedded-sk-in-word",
			in:        "task-a1b2c3d4e5f6g7h8i9j0 is running",
			wantEqual: "task-a1b2c3d4e5f6g7h8i9j0 is running",
		},
		{
			// Same boundary for the prefixed shapes: "task-ant-…" contains
			// "sk-ant-…" mid-word and must not be redacted.
			name:      "key/negative-embedded-sk-ant-in-word",
			in:        "id task-ant-" + strings.Repeat("Z", 24) + " ok",
			wantEqual: "id task-ant-" + strings.Repeat("Z", 24) + " ok",
		},
		{
			// A key right after punctuation still matches, and the captured
			// boundary char is restored by the ${1} replacement.
			name:         "key/positive-sk-after-punct-boundary-preserved",
			in:           "(sk-" + strings.Repeat("G", 24) + ")",
			wantContains: []string{"(" + marker + ")"},
			wantAbsent:   []string{"sk-" + strings.Repeat("G", 24)},
		},

		// -- bearerToken ----------------------------------------------------
		{
			name:         "bearer/positive-header",
			in:           "Authorization: Bearer eyJhbGciOiJ" + strings.Repeat("x", 30),
			wantContains: []string{"Authorization: ", marker},
			wantAbsent:   []string{"eyJhbGciOiJ" + strings.Repeat("x", 30)},
		},
		{
			// Lookalike: the word "Bearer" in prose with no long token.
			name:      "bearer/negative-prose",
			in:        "He is the bearer of bad news.",
			wantEqual: "He is the bearer of bad news.",
		},

		// -- envAssignmentSecret --------------------------------------------
		{
			// Keep the key name, redact the value.
			name:         "env/positive-openai-api-key",
			in:           "OPENAI_API_KEY=" + strings.Repeat("z", 40) + "\nnext",
			wantContains: []string{"OPENAI_API_KEY=", marker, "next"},
			wantAbsent:   []string{strings.Repeat("z", 40)},
		},
		{
			name:         "env/positive-db-password-colon",
			in:           `DB_PASSWORD: "superSecretValue99"`,
			wantContains: []string{"DB_PASSWORD", marker},
			wantAbsent:   []string{"superSecretValue99"},
		},
		{
			// Lookalike: value too short (< 8 chars) to be a real secret.
			name:      "env/negative-short-value",
			in:        "TOKEN=abc",
			wantEqual: "TOKEN=abc",
		},
		{
			// Regression guard (audit, bug #1): the value class used to
			// be `\S{8,}` which swallowed the closing quote/comma and everything to
			// the next whitespace, destroying surrounding JSON/CSV structure. The
			// delimiter-aware class must stop at the quote so the rest survives.
			name:         "env/positive-json-embedded-stops-at-quote",
			in:           `{"env":"OPENAI_API_KEY=` + strings.Repeat("z", 20) + `","next":"value"}`,
			wantContains: []string{"OPENAI_API_KEY=", marker, `"next":"value"}`},
			wantAbsent:   []string{strings.Repeat("z", 20)},
		},
		{
			// Same boundary for a CSV/compact-config comma delimiter.
			name:         "env/positive-csv-embedded-stops-at-comma",
			in:           "API_KEY=" + strings.Repeat("s", 12) + `,"name":"prod"`,
			wantContains: []string{"API_KEY=", marker, `,"name":"prod"`},
			wantAbsent:   []string{strings.Repeat("s", 12)},
		},

		// -- general negative: a short hex string must not be redacted -------
		{
			name:      "negative/short-hex",
			in:        "commit a1b2c3d4 fixes the bug",
			wantEqual: "commit a1b2c3d4 fixes the bug",
		},
		{
			name:      "negative/normal-sentence",
			in:        "The password reset email was sent to the user successfully.",
			wantEqual: "The password reset email was sent to the user successfully.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Scrub(tc.in)
			if tc.wantEqual != "" {
				if got != tc.wantEqual {
					t.Errorf("over/under-redaction\n in: %q\n got: %q\nwant: %q", tc.in, got, tc.wantEqual)
				}
				return
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("missing expected substring %q\n in: %q\ngot: %q", want, tc.in, got)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("secret substring leaked %q\n in: %q\ngot: %q", absent, tc.in, got)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// scrubLongBase64 conditional anchoring.
// ---------------------------------------------------------------------------

func TestScrubLongBase64(t *testing.T) {
	// Note: pemBlock runs before scrubLongBase64 and would consume any real
	// "-----BEGIN ... KEY-----" block (including its "PRIVATE KEY" text). To
	// exercise the "near the literal PRIVATE KEY" path in isolation, the
	// keyword must appear as bare text not inside a PEM block.

	t.Run("long-b64-no-keyword-preserved", func(t *testing.T) {
		// A 300-char base64 blob with no credential keyword nearby (e.g. an
		// image data URI / JWT payload) must be left verbatim.
		blob := makeB64(300)
		in := "data preview: " + blob + " (end)"
		got := Scrub(in)
		if got != in {
			t.Errorf("benign long base64 was redacted\nin:  %q\ngot: %q", in, got)
		}
	})

	t.Run("long-b64-near-private-key-redacted", func(t *testing.T) {
		blob := makeB64(300)
		// "PRIVATE KEY" sits within 100 chars before the blob start.
		in := "PRIVATE KEY body follows: " + blob
		got := Scrub(in)
		if strings.Contains(got, blob) {
			t.Errorf("armor-less key body near PRIVATE KEY was NOT redacted\ngot: %q", got)
		}
		if !strings.Contains(got, marker) {
			t.Errorf("expected redaction marker, got: %q", got)
		}
	})

	t.Run("long-b64-keyword-outside-window-preserved", func(t *testing.T) {
		blob := makeB64(300)
		// "PRIVATE KEY" is more than 100 chars before the blob → outside the
		// look-back window → blob must be kept.
		gap := strings.Repeat("x", 150)
		in := "PRIVATE KEY" + gap + blob
		got := Scrub(in)
		if !strings.Contains(got, blob) {
			t.Errorf("blob redacted despite keyword being outside 100-char window\ngot: %q", got)
		}
	})

	t.Run("long-b64-containing-openssh-armor-redacted", func(t *testing.T) {
		// A long blob that itself contains the OpenSSH armor marker bytes is
		// redacted with no nearby keyword needed.
		const armor = "LS0tLS1CRUdJTiBPUEVOU1NIIFBSSVZBVEUgS0VZ"
		blob := makeB64(150) + armor + makeB64(150)
		in := "junk " + blob + " junk"
		got := Scrub(in)
		if strings.Contains(got, armor) {
			t.Errorf("armor-bearing blob NOT redacted\ngot: %q", got)
		}
	})

	t.Run("double-blob-only-second-near-keyword", func(t *testing.T) {
		// Two long base64 blobs. The FIRST is benign (no keyword). The SECOND
		// sits right after a "PRIVATE KEY" keyword. Per the FindAllStringIndex
		// / per-offset anchoring comment, only the second must be redacted —
		// the first must survive, and they must not merge into one redaction.
		benign := makeB64(250)
		secret := makeB64(260)
		in := "first " + benign + " then PRIVATE KEY: " + secret + " done"
		got := Scrub(in)
		if !strings.Contains(got, benign) {
			t.Errorf("benign first blob was redacted (over-redaction)\ngot: %q", got)
		}
		if strings.Contains(got, secret) {
			t.Errorf("secret second blob (near keyword) was NOT redacted\ngot: %q", got)
		}
		// Exactly one redaction marker expected, and the benign blob's text
		// plus surrounding words must remain intact.
		if strings.Count(got, marker) != 1 {
			t.Errorf("expected exactly 1 redaction, got %d\ngot: %q", strings.Count(got, marker), got)
		}
		if !strings.Contains(got, "first ") || !strings.Contains(got, " done") {
			t.Errorf("surrounding text damaged\ngot: %q", got)
		}
	})

	t.Run("blob-just-under-threshold-preserved", func(t *testing.T) {
		// 199 chars is below the 200-char threshold even with a nearby keyword.
		blob := makeB64(199)
		in := "PRIVATE KEY: " + blob
		got := Scrub(in)
		if !strings.Contains(got, blob) {
			t.Errorf("sub-threshold blob was redacted\ngot: %q", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Idempotency: Scrub(Scrub(x)) == Scrub(x).
// ---------------------------------------------------------------------------

func TestIdempotent(t *testing.T) {
	inputs := []string{
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEbody\n-----END RSA PRIVATE KEY-----",
		"truncated: -----BEGIN RSA PRIVATE KEY-----\nMIIEbody with no END marker",
		"export AWS_SECRET_ACCESS_KEY=" + makeB64(40),
		"xoxb-1234567890-ABCDEFGHIJKL",
		"ghp_" + strings.Repeat("z", 36),
		"PRIVATE KEY body: " + makeB64(300),
		"leaked LS0tLS1CRUdJTiBPUEVOU1NIIFBSSVZBVEUgS0VZLS0tLS0aaaa end",
		"plain text with no secrets at all",
		"两个相邻块 -----BEGIN EC PRIVATE KEY-----\nabc\n-----END EC PRIVATE KEY-----中文",
		"postgres://bob:s3cr3tP4ss@db.host:5432/agent173",
		"sk-ant-api03-" + strings.Repeat("A", 30),
		"Authorization: Bearer eyJhbGci" + strings.Repeat("x", 30),
		"OPENAI_API_KEY=" + strings.Repeat("z", 40),
	}
	for _, in := range inputs {
		once := Scrub(in)
		twice := Scrub(once)
		if once != twice {
			t.Errorf("not idempotent\nin:    %q\nonce:  %q\ntwice: %q", in, once, twice)
		}
	}
}

// ---------------------------------------------------------------------------
// Adjacent PEM blocks must not merge into one swallowing redaction.
// ---------------------------------------------------------------------------

func TestAdjacentPEMBlocks(t *testing.T) {
	between := "<<KEEP-THIS-TEXT>>"
	in := "-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----" +
		between +
		"-----BEGIN EC PRIVATE KEY-----\nBBBB\n-----END EC PRIVATE KEY-----"
	got := Scrub(in)

	if strings.Contains(got, "AAAA") || strings.Contains(got, "BBBB") {
		t.Errorf("PEM body leaked\ngot: %q", got)
	}
	if !strings.Contains(got, between) {
		t.Errorf("lazy match failed: text between two PEM blocks was swallowed\ngot: %q", got)
	}
	// Each block redacted independently → exactly two markers.
	if n := strings.Count(got, marker); n != 2 {
		t.Errorf("expected 2 independent redactions, got %d\ngot: %q", n, got)
	}
}

// ---------------------------------------------------------------------------
// Realistic mixed prompt: Chinese prose + a leaked OpenSSH private key (the
// incident that motivated this module). Key gone, surrounding text intact.
// ---------------------------------------------------------------------------

func TestRealisticMixedPrompt(t *testing.T) {
	pre := "用户问：请帮我看看这个部署脚本有没有问题。下面是日志输出：\n"
	post := "\n以上就是全部内容，请分析。"
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
		"b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gt\n" +
		"ZWQyNTUxOQAAACDsecretsecretsecretsecretsecretsecretsecretAAAAAAEC\n" +
		"-----END OPENSSH PRIVATE KEY-----"
	in := pre + key + post

	got := Scrub(in)

	if !strings.Contains(got, "用户问：请帮我看看这个部署脚本有没有问题") {
		t.Errorf("leading Chinese prose damaged\ngot: %q", got)
	}
	if !strings.Contains(got, "以上就是全部内容，请分析") {
		t.Errorf("trailing Chinese prose damaged\ngot: %q", got)
	}
	if strings.Contains(got, "BEGIN OPENSSH PRIVATE KEY") || strings.Contains(got, "b3BlbnNzaC1rZXkt") {
		t.Errorf("OpenSSH private key leaked through\ngot: %q", got)
	}
	if !strings.Contains(got, marker) {
		t.Errorf("expected redaction marker\ngot: %q", got)
	}
}

// ---------------------------------------------------------------------------
// Empty / no-secret inputs pass through unchanged.
// ---------------------------------------------------------------------------

func TestNoSecretPassthrough(t *testing.T) {
	cases := []string{
		"",
		"hello world",
		"这是一段普通的中文文本，没有任何凭证。",
		"function foo() { return 42; } // normal code",
		"a short base64-ish token: aGVsbG8gd29ybGQ=",
	}
	for _, in := range cases {
		if got := Scrub(in); got != in {
			t.Errorf("no-secret input was altered\nin:  %q\ngot: %q", in, got)
		}
	}
}

// TestEnvAssignmentJSONShape pins the JSON env-dump form (docker inspect /
// kubectl -o json): a quoted key name must still match — the value is redacted,
// the key survives so the log says WHICH secret was present.
func TestEnvAssignmentJSONShape(t *testing.T) {
	in := `{"DB_PASSWORD": "hunter2hunter2secret", "other": "x"}`
	out := Scrub(in)
	if strings.Contains(out, "hunter2hunter2secret") {
		t.Fatalf("JSON-shaped env secret leaked: %q", out)
	}
	if !strings.Contains(out, "DB_PASSWORD") {
		t.Fatalf("key name should survive redaction: %q", out)
	}
}

// TestArmorBase64Rotations pins all three base64 alignment rotations of the
// OpenSSH armor header: a key prefixed by 1-2 foreign bytes before encoding
// must still be caught (standard scanners match all rotations).
func TestArmorBase64Rotations(t *testing.T) {
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMw\n-----END OPENSSH PRIVATE KEY-----"
	for _, prefix := range []string{"", "X", "XY"} {
		enc := base64.StdEncoding.EncodeToString([]byte(prefix + key))
		if out := Scrub(enc); out == enc {
			t.Fatalf("offset-%d rotation leaked through Scrub", len(prefix))
		}
	}
}
