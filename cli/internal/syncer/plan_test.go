package syncer

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(root, rel string) bool {
	_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil
}

func TestValidateRelPath(t *testing.T) {
	bad := []string{"", "/etc/passwd", "../x", "a/../../x", "a/./b", "a//b", `a\b`, "C:/x", ".git/hooks/pre-commit", ".orchestrai/manifest.json", "a/", "x\x00y", "./a"}
	for _, p := range bad {
		var pe *PathError
		if err := ValidateRelPath(p); err == nil || !errors.As(err, &pe) {
			t.Errorf("%q should be rejected, got %v", p, err)
		}
	}
	good := []string{"AGENTS.md", ".claude/skills/x/SKILL.md", ".github/instructions/a.instructions.md", ".gitignore-like/x", "a..b/c"}
	for _, p := range good {
		if err := ValidateRelPath(p); err != nil {
			t.Errorf("%q should be accepted: %v", p, err)
		}
	}
}

func TestResolveSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	realRoot, _ := RealRoot(root)
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(realRoot, "escape/AGENTS.md"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("dir symlink escape not rejected: %v", err)
	}
	writeFile(t, outside, "target.md", "x")
	if err := os.Symlink(filepath.Join(outside, "target.md"), filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(realRoot, "CLAUDE.md"); err == nil {
		t.Fatal("file symlink escape not rejected")
	}
	writeFile(t, root, "docs/real.md", "x")
	if err := os.Symlink("docs/real.md", filepath.Join(root, "link.md")); err != nil {
		t.Fatal(err)
	}
	// Even in-repo links are refused: a link could alias .git/hooks.
	if _, err := Resolve(realRoot, "link.md"); err == nil {
		t.Fatal("in-repo symlink accepted")
	}
	if err := os.MkdirAll(filepath.Join(root, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".git/hooks", filepath.Join(root, "hooks")); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(realRoot, "hooks/pre-commit"); err == nil {
		t.Fatal("symlink into .git accepted")
	}
	got, err := Resolve(realRoot, "docs/real.md")
	if err != nil || got != filepath.Join(realRoot, "docs", "real.md") {
		t.Fatalf("plain path should resolve: %s %v", got, err)
	}
	if err := os.Symlink("nowhere", filepath.Join(root, "dangling.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(realRoot, "dangling.md"); err == nil {
		t.Fatal("dangling symlink accepted")
	}
	writeFile(t, root, "plainfile", "x")
	if _, err := Resolve(realRoot, "plainfile/child.md"); err == nil {
		t.Fatal("file used as directory accepted")
	}
}

func TestBuildPlanRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"../evil.md", "/abs.md", ".git/config", "sub/.git/hooks/x", ".GIT./config", "GIT~1/config", "git~4/config", "ORCHES~2/x", ".git::$INDEX_ALLOCATION/config", "a/b:stream", "c:/x", ".g\u200cit/config"} {
		b := &Bundle{Files: []File{{Path: p, Strategy: StrategyFile, Content: "x"}}}
		if _, err := BuildPlan(root, b, nil, Options{}); err == nil {
			t.Errorf("%s accepted", p)
		}
	}
	if runtime.GOOS != "windows" {
		writeFile(t, root, "AGENTS.md", "x\n")
		if err := os.Symlink("AGENTS.md", filepath.Join(root, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		b := &Bundle{Files: []File{
			{Path: "AGENTS.md", Strategy: StrategyManagedBlock, Content: "a"},
			{Path: "CLAUDE.md", Strategy: StrategyManagedBlock, Content: "@AGENTS.md"},
		}}
		if _, err := BuildPlan(root, b, nil, Options{}); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("aliasing symlink not rejected: %v", err)
		}
	}
}

func TestParseBundleIntegrity(t *testing.T) {
	good := `{"version":7,"hash":"h","files":[{"path":"a.md","tool":"claude","strategy":"file","content":"x","sha256":"` + SHA256Hex([]byte("x")) + `"}]}`
	b, err := ParseBundle([]byte(good))
	if err != nil || b.Version != "7" {
		t.Fatalf("parse: %v %q", err, b.Version)
	}
	for _, bad := range []string{
		`{"files":[{"path":"a","strategy":"file","content":"x","sha256":"00"}]}`,
		`{"files":[{"path":"a","strategy":"weird","content":"x"}]}`,
		`{"files":[{"path":"a","strategy":"file"},{"path":"a","strategy":"file"}]}`,
		`{"files":[{"strategy":"file"}]}`,
	} {
		if _, err := ParseBundle([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func applyBundle(t *testing.T, root string, b *Bundle, opts Options) *Plan {
	t.Helper()
	prev, _, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(root, b, prev, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestManifestDeletionRules(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "AGENTS.md", "# Hand written\n")
	v1 := &Bundle{Version: "1", Hash: "h1", Files: []File{
		{Path: "AGENTS.md", Tool: "agents", Strategy: StrategyManagedBlock, Content: "rules"},
		{Path: ".claude/skills/review/SKILL.md", Tool: "claude", Strategy: StrategyFile, Content: "skill v1\n"},
		{Path: ".claude/skills/old/SKILL.md", Tool: "claude", Strategy: StrategyFile, Content: "old skill\n"},
		{Path: ".cursor/rules/api.mdc", Tool: "cursor", Strategy: StrategyFile, Content: "cursor rule\n"},
		{Path: ".mcp.json", Tool: "claude", Strategy: StrategyJSONMerge, Content: `{"mcpServers":{"t":{"command":"t"}}}`},
	}}
	applyBundle(t, root, v1, Options{})
	writeFile(t, root, ".mcp.json", strings.Replace(readFile(t, root, ".mcp.json"), `"mcpServers": {`, `"mcpServers": {"mine": {"command": "m"},`, 1))
	// Locally edit the cursor rule; it will be dropped and must be kept.
	writeFile(t, root, ".cursor/rules/api.mdc", "cursor rule\nmy tweak\n")

	v2 := &Bundle{Version: "2", Hash: "h2", Files: []File{
		{Path: ".claude/skills/review/SKILL.md", Tool: "claude", Strategy: StrategyFile, Content: "skill v2\n"},
	}}
	prev, _, _ := LoadManifest(root)
	plan, err := BuildPlan(root, v2, prev, Options{})
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]Action{}
	for _, c := range plan.Pending() {
		actions[c.Path] = c.Action
	}
	want := map[string]Action{
		".claude/skills/review/SKILL.md": ActUpdate,
		".claude/skills/old/SKILL.md":    ActDelete,
		"AGENTS.md":                      ActUpdate, // block removed, hand-written text stays
		".mcp.json":                      ActUpdate, // our server removed, "mine" stays
	}
	for p, a := range want {
		if actions[p] != a {
			t.Errorf("%s: got %q want %q (all: %v)", p, actions[p], a, actions)
		}
	}
	if _, ok := actions[".cursor/rules/api.mdc"]; ok {
		t.Error("locally modified dropped file must not be touched")
	}
	if len(plan.Kept) != 1 || plan.Kept[0].Path != ".cursor/rules/api.mdc" {
		t.Errorf("kept = %v", plan.Kept)
	}
	if err := Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	if exists(root, ".claude/skills/old") {
		t.Error("empty skill directory should be removed")
	}
	if got := readFile(t, root, "AGENTS.md"); got != "# Hand written\n" {
		t.Errorf("AGENTS.md = %q", got)
	}
	if got := readFile(t, root, ".mcp.json"); !strings.Contains(got, `"mine"`) || strings.Contains(got, OwnershipKey) {
		t.Errorf(".mcp.json = %s", got)
	}
	if readFile(t, root, ".cursor/rules/api.mdc") != "cursor rule\nmy tweak\n" {
		t.Error("kept file changed")
	}
	m, _, _ := LoadManifest(root)
	if len(m.Files) != 1 || m.BundleVersion != "2" || m.BundleHash != "h2" {
		t.Errorf("manifest = %+v", m)
	}
}

func TestPlanToolFilterAndLocalEdits(t *testing.T) {
	root := t.TempDir()
	b := &Bundle{Version: "1", Hash: "h", Files: []File{
		{Path: "AGENTS.md", Tool: "", Strategy: StrategyManagedBlock, Content: "shared"},
		{Path: ".cursor/mcp.json", Tool: "cursor", Strategy: StrategyJSONMerge, Content: `{"mcpServers":{"a":{"command":"a"}}}`},
		{Path: ".vscode/mcp.json", Tool: "vscode", Strategy: StrategyJSONMerge, Content: `{"servers":{"a":{"command":"a"}}}`},
	}}
	applyBundle(t, root, b, Options{Tools: []string{"cursor"}})
	if exists(root, ".vscode/mcp.json") {
		t.Fatal("vscode file written despite --tools cursor")
	}
	if !exists(root, "AGENTS.md") || !exists(root, ".cursor/mcp.json") {
		t.Fatal("shared or selected files missing")
	}
	// A later sync for vscode only must not drop the cursor entries.
	b2 := &Bundle{Version: "2", Hash: "h2", Files: b.Files[:1]}
	b2.Files = append(b2.Files, b.Files[2])
	plan := applyBundle(t, root, b2, Options{Tools: []string{"vscode"}})
	if !exists(root, ".cursor/mcp.json") {
		t.Fatal("cursor file removed by a vscode-only sync")
	}
	if _, ok := plan.Manifest.Files[".cursor/mcp.json"]; !ok {
		t.Fatal("unselected tool entry must be carried over in the manifest")
	}

	// Local edits inside the managed block are detected and replaced.
	writeFile(t, root, "AGENTS.md", strings.Replace(readFile(t, root, "AGENTS.md"), "shared", "hacked", 1))
	m, _, _ := LoadManifest(root)
	states, err := LocalStatus(root, m)
	if err != nil {
		t.Fatal(err)
	}
	if Clean(states) {
		t.Fatal("drift not detected")
	}
	b3 := &Bundle{Version: "3", Hash: "h3", Files: []File{{Path: "AGENTS.md", Strategy: StrategyManagedBlock, Content: "shared v3"}, b.Files[2]}}
	plan3, err := BuildPlan(root, b3, m, Options{Tools: []string{"vscode"}})
	if err != nil {
		t.Fatal(err)
	}
	var flagged bool
	for _, c := range plan3.Changes {
		if c.Path == "AGENTS.md" && c.LocalEdits {
			flagged = true
		}
	}
	if !flagged {
		t.Fatal("local edits not flagged")
	}
}

func TestOutOfSyncAndIdempotentApply(t *testing.T) {
	root := t.TempDir()
	b := &Bundle{Version: "1", Hash: "h", Files: []File{{Path: "AGENTS.md", Strategy: StrategyManagedBlock, Content: "x"}}}
	prev, existsPrev, _ := LoadManifest(root)
	plan, err := BuildPlan(root, b, prev, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.OutOfSync(prev, existsPrev) {
		t.Fatal("fresh repo should be out of sync")
	}
	if err := Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	prev, existsPrev, _ = LoadManifest(root)
	plan, _ = BuildPlan(root, b, prev, Options{})
	if plan.OutOfSync(prev, existsPrev) || len(plan.Pending()) != 0 {
		t.Fatal("second plan should be a no-op")
	}
}

func TestUnifiedDiff(t *testing.T) {
	a := "l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\nl10\n"
	b := "l1\nl2\nCHANGED\nl4\nl5\nl6\nl7\nl8\nl9\nl10\nl11\n"
	got := UnifiedDiff("a/x", "b/x", a, b)
	want := "--- a/x\n+++ b/x\n@@ -1,6 +1,6 @@\n l1\n l2\n-l3\n+CHANGED\n l4\n l5\n l6\n@@ -8,3 +8,4 @@\n l8\n l9\n l10\n+l11\n"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if UnifiedDiff("a", "b", "same\r\n", "same\n") != "" {
		t.Fatal("CRLF-only difference should not diff")
	}
	created := ChangeDiff(Change{Path: "n.md", Action: ActCreate, After: []byte("new")})
	if !strings.Contains(created, "--- /dev/null") || !strings.Contains(created, "@@ -0,0 +1 @@") || !strings.Contains(created, "\\ No newline at end of file") {
		t.Fatalf("create diff: %s", created)
	}
}
