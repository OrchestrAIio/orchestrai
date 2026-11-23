package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OrchestrAIio/orchestrai/cli/internal/syncer"
)

func TestAddAnonymousAutoFallbackAndLocalFlag(t *testing.T) {
	fb := newFakeBackend(t)
	repo := setupRepo(t)
	app := newTestApp(t, fb, repo)
	app.Prog = NpxProg

	// Brand-new anonymous user runs `add acme/pr-review` without signing in or binding.
	if code := app.run(t, "add", "acme/pr-review"); code != 0 {
		t.Fatalf("anonymous add failed (%d): %s %s", code, app.out, app.errOut)
	}
	out := app.out.String()
	if !strings.Contains(out, "Installed acme/pr-review@1.2.0 locally (no sign-in required).") {
		t.Fatalf("missing local install confirmation:\n%s", out)
	}
	if !strings.Contains(out, "Sync rules, skills & MCP servers across your whole team & CI (free for up to 5): npx @orchestrai/cli login") {
		t.Fatalf("missing login funnel CTA:\n%s", out)
	}
	// Verify no auth header was sent and no server-side install was created.
	if len(fb.publicAuthHdr) != 1 || fb.publicAuthHdr[0] != "" {
		t.Fatalf("expected anonymous fetch with empty Authorization, got %q", fb.publicAuthHdr)
	}
	if len(fb.installs) != 0 {
		t.Fatalf("anonymous add must not call CreateInstall: %v", fb.installs)
	}
	// Verify files were written to both Claude and shared skills locations.
	for _, rel := range []string{".claude/skills/pr-review/SKILL.md", ".agents/skills/pr-review/SKILL.md"} {
		data, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(rel)))
		if err != nil || !strings.Contains(string(data), "Review carefully.") {
			t.Fatalf("missing %s: %v %q", rel, err, string(data))
		}
	}
	// Verify manifest was updated.
	m, ok, err := syncer.LoadManifest(repo)
	if err != nil || !ok {
		t.Fatalf("manifest not written: %v", err)
	}
	if m.Files[".claude/skills/pr-review/SKILL.md"] == "" || m.Files[".agents/skills/pr-review/SKILL.md"] == "" {
		t.Fatalf("manifest missing skill entries: %+v", m.Files)
	}

	// Add a second item locally (rule + mcp_server + workflow + subagent + prompt + doc) and verify prior installs are preserved.
	fb.mu.Lock()
	fb.publicItems["acme/api-rule"] = map[string]any{
		"id": "item-2", "publisher": "acme", "slug": "api-rule", "name": "API Rule",
		"kind": "rule", "latest_version": "1.0.0", "description": "Validate inputs",
		"body": map[string]any{"body_md": "Always validate input with zod.", "path_glob": "src/api/**"},
	}
	fb.publicItems["acme/github-mcp"] = map[string]any{
		"id": "item-3", "publisher": "acme", "slug": "github-mcp", "name": "GitHub MCP",
		"kind": "mcp_server", "latest_version": "2.0.0", "description": "GitHub tools",
		"body": map[string]any{"transport": "stdio", "command": "npx", "args": []string{"-y", "@modelcontextprotocol/server-github"}, "env": map[string]string{"GITHUB_TOKEN": "${GITHUB_TOKEN}"}},
	}
	fb.publicItems["acme/release-wf"] = map[string]any{
		"id": "item-4", "publisher": "acme", "slug": "release-wf", "name": "Release",
		"kind": "workflow", "latest_version": "1.1.0", "description": "Cut a release",
		"body": map[string]any{"body_md": "Run tests and tag release."},
	}
	fb.publicItems["acme/sec-agent"] = map[string]any{
		"id": "item-5", "publisher": "acme", "slug": "sec-agent", "name": "Sec Agent",
		"kind": "subagent", "latest_version": "0.9.0", "description": "Security reviewer",
		"body": map[string]any{"body_md": "Audit auth and permissions."},
	}
	fb.publicItems["acme/arch-doc"] = map[string]any{
		"id": "item-6", "publisher": "acme", "slug": "arch-doc", "name": "Arch Doc",
		"kind": "doc", "latest_version": "1.0.0", "description": "System design",
		"body": map[string]any{"url": "https://docs.example.test/arch", "purpose": "Core architecture"},
	}
	fb.mu.Unlock()

	for _, ref := range []string{"acme/api-rule", "acme/github-mcp", "acme/release-wf", "acme/sec-agent", "acme/arch-doc"} {
		if code := app.run(t, "add", ref, "--local"); code != 0 {
			t.Fatalf("local add %s failed (%d): %s %s", ref, code, app.out, app.errOut)
		}
	}

	// Verify previously installed skill is STILL on disk and in manifest!
	if _, err := os.Stat(filepath.Join(repo, ".claude/skills/pr-review/SKILL.md")); err != nil {
		t.Fatalf("previous skill was deleted: %v", err)
	}
	// Verify rule files + AGENTS.md preserved hand-written text and merged both rule and doc.
	agentsData, _ := os.ReadFile(filepath.Join(repo, "AGENTS.md"))
	for _, want := range []string{"Hand-written guidance stays.", "Always validate input with zod.", "https://docs.example.test/arch"} {
		if !strings.Contains(string(agentsData), want) {
			t.Fatalf("AGENTS.md missing %q:\n%s", want, string(agentsData))
		}
	}
	for _, rel := range []string{
		".cursor/rules/api-rule.mdc",
		".github/instructions/api-rule.instructions.md",
		".mcp.json",
		".cursor/mcp.json",
		".vscode/mcp.json",
		".codex/config.toml",
		".claude/commands/release-wf.md",
		".cursor/commands/release-wf.md",
		".github/prompts/release-wf.prompt.md",
		".claude/agents/sec-agent.md",
		".codex/agents/sec-agent.toml",
	} {
		if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("expected file %s not created: %v", rel, err)
		}
	}
	// Verify .vscode/mcp.json kept user's local-db server and added github-mcp with ${env:GITHUB_TOKEN}.
	vscodeMCP, _ := os.ReadFile(filepath.Join(repo, ".vscode/mcp.json"))
	if !strings.Contains(string(vscodeMCP), "local-db") || !strings.Contains(string(vscodeMCP), "${env:GITHUB_TOKEN}") {
		t.Fatalf(".vscode/mcp.json content unexpected:\n%s", string(vscodeMCP))
	}

	// Signed in + bound repo STILL does team install unless --local is passed.
	app.login(t)
	if code := app.run(t, "init", "--project", "p1"); code != 0 {
		t.Fatalf("init: %d %s", code, app.errOut)
	}
	if code := app.run(t, "add", "acme/pr-review", "--local", "--json"); code != 0 {
		t.Fatalf("explicit --local while bound failed: %d %s", code, app.errOut)
	}
	var localJSON map[string]any
	if err := json.Unmarshal(app.out.Bytes(), &localJSON); err != nil || localJSON["local"] != true {
		t.Fatalf("--local --json output: %v %s", err, app.out)
	}
	if len(fb.installs) != 0 {
		t.Fatalf("--local must not create server install")
	}

	// Without --local while signed in + bound, team install works as before.
	if code := app.run(t, "add", "acme/pr-review"); code != 0 {
		t.Fatalf("team add failed: %d %s", code, app.errOut)
	}
	if len(fb.installs) != 1 || !strings.Contains(app.out.String(), "into the team space.") {
		t.Fatalf("expected team space install, installs=%d out=%s", len(fb.installs), app.out)
	}
}

func TestCheckCommandScansAllCategories(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("Dockerfile", "FROM node:latest\nRUN curl -fsSL https://example.com/install.sh | sh\nCMD [\"node\", \"index.js\"]\n")
	write("infra/main.tf", `
resource "aws_security_group" "web" {
  ingress {
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
}
resource "aws_db_instance" "db" {
  publicly_accessible = true
  storage_encrypted   = false
}
`)
	write("k8s/deploy.yaml", `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  template:
    spec:
      containers:
        - name: web
          image: nginx:1.25
          securityContext:
            privileged: true
`)
	write(".github/workflows/ci.yml", `
name: CI
permissions: write-all
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@main
      - uses: actions/setup-node@v4
`)
	write(".env", "DATABASE_URL=postgres://user:pass@localhost/db\n")
	write(".env.example", "DATABASE_URL=postgres://user:pass@localhost/db\n")
	write("config.go", "package cfg\nconst k = \"AKIAIOSFODNN7EXAMPLE\"\nconst g = \"ghp_1234567890abcdefghij123456\"\nconst s = \"sk_live_1234567890abcdef12\"\n")
	write("key.pem", "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\n")

	fb := newFakeBackend(t)
	app := newTestApp(t, fb, dir)
	// Works offline and anonymously (no login, no repo binding).
	if code := app.run(t, "check"); code != 0 {
		t.Fatalf("check exit %d: %s", code, app.errOut)
	}
	out := app.out.String()
	for _, want := range []string{
		"dockerfile-latest-tag",
		"dockerfile-missing-user",
		"dockerfile-curl-pipe-sh",
		"tf-open-ingress",
		"tf-publicly-accessible",
		"tf-unencrypted",
		"k8s-privileged-container",
		"k8s-missing-resource-limits",
		"gha-write-all-permissions",
		"gha-unpinned-action",
		"committed-env-file",
		"hardcoded-aws-key",
		"hardcoded-github-token",
		"hardcoded-stripe-key",
		"hardcoded-private-key",
		"Have OrchestrAI fix these and open a PR (from $5 = 100 credits, or Pro $29/mo): orchestrai ask \"Fix the readiness issues found by orchestrai check\"",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("check output missing %q:\n%s", want, out)
		}
	}
	// .env.example must NOT be flagged as committed-env-file.
	if strings.Contains(out, ".env.example:") {
		t.Errorf(".env.example should be ignored:\n%s", out)
	}
	// actions/setup-node@v4 must NOT be flagged as unpinned.
	if strings.Contains(out, "actions/setup-node@v4") {
		t.Errorf("actions/setup-node@v4 should not be flagged:\n%s", out)
	}

	// JSON output + --exit-code.
	if code := app.run(t, "check", "--json", "--exit-code"); code != ExitOutOfSync {
		t.Fatalf("check --exit-code = %d, want 1", code)
	}
	var rep CheckReport
	if err := json.Unmarshal(app.out.Bytes(), &rep); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, app.out)
	}
	if rep.Counts["total"] < 12 || rep.Counts["critical"] < 4 {
		t.Fatalf("unexpected counts: %+v", rep.Counts)
	}

	// Clean repo produces zero findings.
	cleanDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(cleanDir, "Dockerfile"), []byte("FROM node:22-alpine\nUSER node\nCMD [\"node\"]\n"), 0o644)
	_ = os.WriteFile(filepath.Join(cleanDir, ".env.template"), []byte("API_KEY=changeme\n"), 0o644)
	if code := app.run(t, "check", cleanDir, "--exit-code"); code != 0 {
		t.Fatalf("clean repo check failed (%d): %s %s", code, app.out, app.errOut)
	}
	if !strings.Contains(app.out.String(), "No readiness issues found") {
		t.Fatalf("clean output: %s", app.out)
	}
}

func TestAuditCommand(t *testing.T) {
	fb := newFakeBackend(t)
	repo := setupRepo(t)
	app := newTestApp(t, fb, repo)
	app.login(t)
	app.run(t, "init", "--project", "p1")

	// Unconnected cloud accounts.
	if code := app.run(t, "audit"); code != 0 {
		t.Fatalf("audit unconnected: %d %s", code, app.errOut)
	}
	if !strings.Contains(app.out.String(), "Connect AWS, GCP, or Azure with read-only permissions in ~60 seconds") {
		t.Fatalf("unexpected unconnected output:\n%s", app.out)
	}

	// Connected cloud accounts using backend-go's exact nested LiveAuditSummary shape.
	fb.mu.Lock()
	fb.auditSummary = map[string]any{
		"project_id":           "p1",
		"has_cloud_connection": true,
		"cost": map[string]any{
			"available":           true,
			"findings_count":      2,
			"monthly_savings_usd": 420.5,
			"top_findings": []map[string]any{
				{"resource_id": "nat-gateway-prod", "provider": "aws", "monthly_savings_usd": 320.5, "title": "Idle NAT gateway with zero traffic"},
				{"resource_id": "db-staging", "provider": "gcp", "monthly_savings_usd": 100.0, "title": "Overprovisioned Cloud SQL instance"},
			},
		},
		"security": map[string]any{
			"available": true,
			"pass":      40,
			"fail":      3,
			"failed_controls": []map[string]any{
				{"control_id": "CIS-2.1", "severity": "high", "title": "S3 bucket allows public read", "resource": "s3://acme-assets"},
			},
		},
		"upgrade_url": "/settings/billing?buy=credits&from=live_audit",
	}
	fb.mu.Unlock()

	if code := app.run(t, "audit"); code != 0 {
		t.Fatalf("audit connected: %d %s", code, app.errOut)
	}
	out := app.out.String()
	for _, want := range []string{
		"Cloud Audit Summary (aws, gcp)",
		"$420.50/mo",
		"Failed security controls:       3",
		"nat-gateway-prod",
		"S3 bucket allows public read",
		"Fix with OrchestrAI Agent",
		"https://app.test/settings/billing?buy=credits&from=live_audit",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("audit output missing %q:\n%s", want, out)
		}
	}

	// --json mode.
	if code := app.run(t, "audit", "--json"); code != 0 {
		t.Fatalf("audit --json: %d %s", code, app.errOut)
	}
	var parsed map[string]any
	if err := json.Unmarshal(app.out.Bytes(), &parsed); err != nil || parsed["connected"] != true {
		t.Fatalf("audit json: %v %s", err, app.out)
	}
}
