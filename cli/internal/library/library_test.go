package library

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareInfersKinds(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "skills/pr-review/SKILL.md"), "---\nname: PR Review\ndescription: Reviews pull requests\n---\nDo it.\n")
	write(t, filepath.Join(dir, "skills/pr-review/scripts/run.sh"), "#!/bin/sh\necho hi\n")
	write(t, filepath.Join(dir, "skills/pr-review/assets/logo.bin"), "\x00\x01\x02")
	write(t, filepath.Join(dir, ".claude/commands/deploy.md"), "---\ndescription: Deploy\nallowed-tools: Bash\n---\nDeploy $ARGUMENTS\n")
	write(t, filepath.Join(dir, ".claude/agents/sec.md"), "---\nname: security-reviewer\ndescription: Finds vulns\ntools: Read, Grep\n---\nYou review.\n")
	write(t, filepath.Join(dir, "rules/api.mdc"), "---\nglobs: src/api/**\n---\nUse zod.\n")
	write(t, filepath.Join(dir, "x/fix.prompt.md"), "Fix it\n")
	write(t, filepath.Join(dir, "notes/plain.md"), "A prompt\n")
	write(t, filepath.Join(dir, "servers/db.json"), `{"name":"db","command":"npx","env":{"DB_URL":"${env:DB_URL}"}}`)

	tests := []struct {
		path, kind, name string
		files            int
	}{
		{"skills/pr-review", "skill", "PR Review", 3},
		{"skills/pr-review/SKILL.md", "skill", "PR Review", 3},
		{".claude/commands/deploy.md", "workflow", "deploy", 1},
		{".claude/agents/sec.md", "subagent", "security-reviewer", 1},
		{"rules/api.mdc", "rule", "api", 1},
		{"x/fix.prompt.md", "workflow", "fix", 1},
		{"notes/plain.md", "prompt", "plain", 1},
		{"servers/db.json", "mcp_server", "db", 1},
	}
	for _, tc := range tests {
		p, err := Prepare(filepath.Join(dir, tc.path), "")
		if err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		if p.Kind != tc.kind || p.Name != tc.name || len(p.Files) != tc.files {
			t.Errorf("%s → kind=%s name=%q files=%d", tc.path, p.Kind, p.Name, len(p.Files))
		}
	}
	p, _ := Prepare(filepath.Join(dir, "skills/pr-review"), "")
	var sawBinary bool
	for _, f := range p.Files {
		if f.Path == "assets/logo.bin" && f.Encoding == "base64" {
			sawBinary = true
		}
	}
	if !sawBinary || p.Slug != "pr-review" {
		t.Errorf("binary encoding or slug wrong: %+v", p)
	}
	doc, err := Prepare("https://docs.test/runbook", "")
	if err != nil || doc.Kind != "doc" || doc.URL == "" {
		t.Errorf("doc: %+v %v", doc, err)
	}
	if _, err := Prepare(filepath.Join(dir, "notes"), ""); err == nil {
		t.Error("directory without SKILL.md accepted")
	}
	if _, err := Prepare(filepath.Join(dir, "notes/plain.md"), "bogus"); err == nil {
		t.Error("invalid kind accepted")
	}
}

func TestPrepareRefusesSecrets(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"aws.md":     "key AKIA" + "IOSFODNN7EXAMPLF in text\n",
		"gh.md":      "token: ghp_" + strings.Repeat("a1B2", 9) + "\n",
		"pem.md":     "-----BEGIN RSA PRIVATE KEY-----\nMIIE\n",
		"generic.md": "api_key = \"q8Zr2LmP0xV7nT4kW9sB\"\n",
		"anth.md":    "sk-ant-api03-" + strings.Repeat("Ab9_", 8) + "\n",
		"url.md":     "postgres://admin:hunter2pass@db.internal/app\n",
	}
	for name, content := range cases {
		p := filepath.Join(dir, name)
		write(t, p, content)
		_, err := Prepare(p, "prompt")
		var se *SecretsError
		if !errors.As(err, &se) || len(se.Findings) == 0 {
			t.Errorf("%s: expected secrets error, got %v", name, err)
			continue
		}
		if strings.Contains(se.Error(), strings.TrimSpace(content)) {
			t.Errorf("%s: error must redact the secret", name)
		}
	}
	safe := map[string]string{
		"placeholder.md": "api_key = \"${env:OPENAI_API_KEY}\"\npassword: \"<your-password-here>\"\n",
		"pragma.md":      "token: \"ghp_" + strings.Repeat("a1B2", 9) + "\" # orchestrai:allow-secret\n",
		"lowentropy.md":  "secret = \"aaaaaaaaaaaaaaaaaaaa\"\n",
	}
	for name, content := range safe {
		p := filepath.Join(dir, name)
		write(t, p, content)
		if _, err := Prepare(p, "prompt"); err != nil {
			t.Errorf("%s: false positive: %v", name, err)
		}
	}
	mcp := filepath.Join(dir, "srv.json")
	write(t, mcp, `{"name":"s","command":"x","env":{"API_TOKEN":"abc123literal"}}`)
	if _, err := Prepare(mcp, ""); err == nil {
		t.Error("literal MCP env secret accepted")
	}
}

func TestParseFrontmatter(t *testing.T) {
	fm := ParseFrontmatter("---\r\nname: \"x\"\r\ntools: [Read, 'Grep']\r\npaths:\r\n  - src/**\r\n  - lib/**\r\n---\r\nbody")
	if fm["name"] != "x" {
		t.Errorf("name = %v", fm["name"])
	}
	if tl, _ := fm["tools"].([]string); len(tl) != 2 || tl[1] != "Grep" {
		t.Errorf("tools = %v", fm["tools"])
	}
	if pl, _ := fm["paths"].([]string); len(pl) != 2 {
		t.Errorf("paths = %v", fm["paths"])
	}
	if len(ParseFrontmatter("no front matter")) != 0 {
		t.Error("expected empty map")
	}
}
