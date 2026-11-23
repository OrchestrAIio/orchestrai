// Package library prepares local files for `orchestrai library publish`:
// reading a skill directory or a single agent file, inferring the item kind,
// and refusing uploads that contain secrets.
package library

import (
	"bufio"
	"fmt"
	"regexp"
	"strings"
)

// AllowPragma on a line suppresses secret findings for that line.
const AllowPragma = "orchestrai:allow-secret"

type secretRule struct {
	name string
	re   *regexp.Regexp
	// generic rules skip obvious placeholders.
	generic bool
}

var secretRules = []secretRule{
	{name: "private key", re: regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP |ENCRYPTED )?PRIVATE KEY(?: BLOCK)?-----`)},
	{name: "AWS access key id", re: regexp.MustCompile(`\b(?:AKIA|ASIA|ABIA|ACCA)[0-9A-Z]{16}\b`)},
	{name: "AWS secret access key", re: regexp.MustCompile(`(?i)aws_?secret_?access_?key["'\s]*[:=]\s*["']?[A-Za-z0-9/+=]{40}\b`)},
	{name: "GitHub token", re: regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,}\b|\bgithub_pat_[A-Za-z0-9_]{60,}\b`)},
	{name: "GitLab token", re: regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{20,}\b`)},
	{name: "Slack token", re: regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}\b`)},
	{name: "Slack webhook", re: regexp.MustCompile(`https://hooks\.slack\.com/services/T[A-Z0-9]+/B[A-Z0-9]+/[A-Za-z0-9]+`)},
	{name: "Anthropic API key", re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{20,}\b`)},
	{name: "OpenAI API key", re: regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_\-]{32,}\b`)},
	{name: "OpenRouter API key", re: regexp.MustCompile(`\bsk-or-v1-[a-f0-9]{48,}\b`)},
	{name: "Google API key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`)},
	{name: "Stripe secret key", re: regexp.MustCompile(`\b(?:sk|rk)_(?:live|test)_[0-9A-Za-z]{20,}\b`)},
	{name: "npm token", re: regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`)},
	{name: "Hugging Face token", re: regexp.MustCompile(`\bhf_[A-Za-z0-9]{30,}\b`)},
	{name: "OrchestrAI API key", re: regexp.MustCompile(`\bork_[A-Za-z0-9_\-]{20,}\b`)},
	{name: "JSON web token", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\b`)},
	{name: "credentials in URL", re: regexp.MustCompile(`[a-z][a-z0-9+.\-]*://[^/\s:@"']+:[^/\s@"'$<{]{6,}@[^\s"']+`)},
	{name: "hard-coded secret", generic: true, re: regexp.MustCompile(`(?i)\b[a-z0-9_.\-]*(?:api[_\-]?key|secret|token|passw(?:or)?d|pwd|auth[_\-]?key|access[_\-]?key|client[_\-]?secret)[a-z0-9_.\-]*["']?\s*[:=]\s*["']([^"'\s]{16,})["']`)},
}

var placeholderHints = []string{"${", "$(", "<", "{{", "xxxx", "****", "example", "placeholder", "changeme", "your_", "your-", "dummy", "redacted", "env:", "process.env", "os.environ"}

func isPlaceholder(v string) bool {
	l := strings.ToLower(v)
	for _, h := range placeholderHints {
		if strings.Contains(l, h) {
			return true
		}
	}
	// Low variety strings (aaaaaaaa…, 00000000…) are not real secrets.
	distinct := map[rune]bool{}
	for _, r := range v {
		distinct[r] = true
	}
	return len(distinct) < 6
}

// Finding is one suspected secret.
type Finding struct {
	Path string
	Line int
	Rule string
	Snip string // redacted
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %s (%s)", f.Path, f.Line, f.Rule, f.Snip)
}

func redact(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + strings.Repeat("*", 8) + s[len(s)-2:]
}

// ScanSecrets scans text content for secrets.
func ScanSecrets(path, content string) []Finding {
	var out []Finding
	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	ln := 0
	for sc.Scan() {
		ln++
		line := sc.Text()
		if strings.Contains(line, AllowPragma) {
			continue
		}
		for _, r := range secretRules {
			m := r.re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			val := m[0]
			if r.generic {
				val = m[1]
				if isPlaceholder(val) {
					continue
				}
			}
			out = append(out, Finding{Path: path, Line: ln, Rule: r.name, Snip: redact(val)})
			break
		}
	}
	return out
}
