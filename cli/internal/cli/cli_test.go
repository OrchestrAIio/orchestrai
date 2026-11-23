package cli

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/OrchestrAIio/orchestrai/cli/internal/api"
	"github.com/OrchestrAIio/orchestrai/cli/internal/creds"
)

var update = flag.Bool("update", false, "rewrite golden files")

// setupRepo creates a git-like repo with hand-written files and returns it.
func setupRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"AGENTS.md":        "# Project notes\n\nHand-written guidance stays.\n",
		".vscode/mcp.json": "{\n  \"servers\": {\n    \"local-db\": {\n      \"type\": \"stdio\",\n      \"command\": \"db-mcp\"\n    }\n  }\n}\n",
		"src/main.go":      "package main\n",
	}
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestGoldenEndToEnd(t *testing.T) {
	fb := newFakeBackend(t)
	repo := setupRepo(t)
	app := newTestApp(t, fb, filepath.Join(repo, "src"))

	app.login(t)
	if !strings.Contains(app.out.String(), "Signed in as Ada Lovelace <ada@example.test>") {
		t.Fatalf("login output: %s", app.out)
	}
	if !strings.Contains(app.errOut.String(), "WXYZ-1234") {
		t.Fatalf("login must show the user code: %s", app.errOut)
	}
	// File-backed credentials are private.
	if fi, err := os.Stat(app.Store.Path()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file mode: %v %v", fi, err)
	}

	if code := app.run(t, "init", "--project", "p1"); code != 0 {
		t.Fatalf("init exit %d: %s", code, app.errOut)
	}
	if code := app.run(t, "sync"); code != 0 {
		t.Fatalf("sync exit %d: %s%s", code, app.out, app.errOut)
	}
	if !strings.Contains(app.out.String(), "Synced bundle 1: 6 created, 2 updated, 0 removed.") {
		t.Fatalf("sync summary: %s", app.out)
	}

	tree := readTree(t, repo)
	for k, v := range tree {
		tree[k] = strings.ReplaceAll(v, fb.srv.URL, "{{API_URL}}")
	}
	compareGolden(t, filepath.Join("testdata", "golden"), tree)

	// The sync report was sent.
	if len(fb.reports) != 1 || fb.reports[0]["client"] != "ork" || fb.reports[0]["manifest_hash"] == "" {
		t.Fatalf("reports = %v", fb.reports)
	}

	// A second sync is a conditional GET → 304.
	if code := app.run(t, "sync"); code != 0 || !strings.Contains(app.out.String(), "Already up to date") {
		t.Fatalf("second sync: %d %s %s", code, app.out, app.errOut)
	}
	if fb.notModified != 1 {
		t.Fatalf("expected one 304, got %d", fb.notModified)
	}

	// CI check passes when in sync.
	if code := app.run(t, "sync", "--check"); code != ExitOK {
		t.Fatalf("check in sync: %d %s", code, app.out)
	}
	// status is clean.
	if code := app.run(t, "status", "--json", "--exit-code"); code != ExitOK {
		t.Fatalf("status: %d %s %s", code, app.out, app.errOut)
	}
}

func compareGolden(t *testing.T, dir string, tree map[string]string) {
	t.Helper()
	if *update {
		_ = os.RemoveAll(dir)
		for rel, content := range tree {
			p := filepath.Join(dir, filepath.FromSlash(rel)+".golden")
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	want := map[string]string{}
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		data, err := os.ReadFile(p)
		want[strings.TrimSuffix(filepath.ToSlash(rel), ".golden")] = string(data)
		return err
	})
	if err != nil {
		t.Fatalf("read golden (run go test -update): %v", err)
	}
	var keys []string
	for k := range want {
		keys = append(keys, k)
	}
	for k := range tree {
		if _, ok := want[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if tree[k] != want[k] {
			t.Errorf("%s differs from golden:\n--- got\n%s\n--- want\n%s", k, tree[k], want[k])
		}
	}
}

func TestSyncCheckDetectsDriftAndBundleChanges(t *testing.T) {
	fb := newFakeBackend(t)
	repo := setupRepo(t)
	app := newTestApp(t, fb, repo)
	app.login(t)
	app.run(t, "init", "--project", "p1")
	if code := app.run(t, "sync"); code != 0 {
		t.Fatalf("sync: %s", app.errOut)
	}

	// Local edit inside a managed block → check fails, status reports drift.
	agents := filepath.Join(repo, "AGENTS.md")
	data, _ := os.ReadFile(agents)
	if err := os.WriteFile(agents, []byte(strings.Replace(string(data), "never npm", "or npm", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := app.run(t, "status", "--offline", "--exit-code"); code != ExitOutOfSync || !strings.Contains(app.out.String(), "modified  AGENTS.md") {
		t.Fatalf("status drift: %d %s", code, app.out)
	}
	if code := app.run(t, "sync", "--check", "--json"); code != ExitOutOfSync {
		t.Fatalf("check with drift: %d %s %s", code, app.out, app.errOut)
	}
	var res syncResult
	if err := json.Unmarshal(app.out.Bytes(), &res); err != nil || res.InSync || len(res.Changes) != 1 || res.Changes[0].Path != "AGENTS.md" {
		t.Fatalf("check json: %v %+v", err, res)
	}
	// diff shows the restore.
	if code := app.run(t, "diff", "--exit-code"); code != ExitOutOfSync || !strings.Contains(app.out.String(), "-- Use pnpm, or npm.") || !strings.Contains(app.out.String(), "+- Use pnpm, never npm.") {
		t.Fatalf("diff: %d %s", code, app.out)
	}
	// --dry-run writes nothing.
	app.run(t, "sync", "--dry-run")
	if after, _ := os.ReadFile(agents); !strings.Contains(string(after), "or npm") {
		t.Fatal("dry-run modified files")
	}
	if code := app.run(t, "sync"); code != 0 || !strings.Contains(app.errOut.String(), "local edits") {
		t.Fatalf("repair sync: %d %s", code, app.errOut)
	}
	last := fb.reports[len(fb.reports)-1]
	if drift, _ := last["drift"].([]any); len(drift) != 1 {
		t.Fatalf("drift not reported: %v", last)
	}

	// Server-side change: a rule is dropped and the remote bundle moves on.
	fb.bump(func(files []bundleFile) []bundleFile {
		var out []bundleFile
		for _, f := range files {
			if f.Path != ".cursor/rules/backend.mdc" {
				out = append(out, f)
			}
		}
		return out
	})
	if code := app.run(t, "status", "--json", "--exit-code"); code != ExitOutOfSync {
		t.Fatalf("stale bundle not reported: %d %s", code, app.out)
	}
	if code := app.run(t, "sync", "--check"); code != ExitOutOfSync || !strings.Contains(app.out.String(), "would delete") {
		t.Fatalf("check: %d %s", code, app.out)
	}
	if code := app.run(t, "sync"); code != 0 {
		t.Fatal(app.errOut.String())
	}
	if _, err := os.Stat(filepath.Join(repo, ".cursor", "rules")); !os.IsNotExist(err) {
		t.Fatal("dropped rule (and its empty dir) should be removed")
	}
	if code := app.run(t, "sync", "--check"); code != ExitOK {
		t.Fatalf("after sync: %d %s", code, app.out)
	}
}

func TestSyncToolsFilter(t *testing.T) {
	fb := newFakeBackend(t)
	repo := setupRepo(t)
	app := newTestApp(t, fb, repo)
	app.login(t)
	app.run(t, "init", "--project", "p1")
	if code := app.run(t, "sync", "--tools", "cursor"); code != 0 {
		t.Fatal(app.errOut.String())
	}
	tree := readTree(t, repo)
	for _, p := range []string{"AGENTS.md", ".cursor/mcp.json", ".cursor/rules/backend.mdc"} {
		if _, ok := tree[p]; !ok {
			t.Errorf("missing %s", p)
		}
	}
	for _, p := range []string{".mcp.json", "CLAUDE.md", ".codex/config.toml", ".github/instructions/backend.instructions.md"} {
		if _, ok := tree[p]; ok {
			t.Errorf("%s written despite --tools cursor", p)
		}
	}
	if strings.Contains(tree[".vscode/mcp.json"], "orchestrai") {
		t.Error("vscode config touched")
	}
	if code := app.run(t, "sync", "--tools", "Bad Tool"); code != ExitUsage {
		t.Fatalf("invalid tool accepted: %d", code)
	}
}

func TestLimitAndAuthErrors(t *testing.T) {
	fb := newFakeBackend(t)
	repo := setupRepo(t)
	app := newTestApp(t, fb, repo)

	// Not signed in.
	if code := app.run(t, "sync", "--project", "p1"); code != ExitAuth {
		t.Fatalf("unauthenticated sync: %d %s", code, app.errOut)
	}
	// Not bound.
	app.login(t)
	if code := app.run(t, "sync"); code != ExitNotBound || !strings.Contains(app.errOut.String(), "orchestrai init") {
		t.Fatalf("unbound: %d %s", code, app.errOut)
	}
	app.run(t, "init", "--project", "p1")

	fb.mu.Lock()
	fb.bundleLimit = true
	fb.mu.Unlock()
	if code := app.run(t, "sync"); code != ExitLimit {
		t.Fatalf("402: %d", code)
	}
	msg := app.errOut.String()
	if !strings.Contains(msg, "The free plan includes 1 synced repo.") || !strings.Contains(msg, "Upgrade your plan: https://app.test/settings/billing?from=live_hub_repos") || !strings.Contains(msg, "Usage: 1 of 1") {
		t.Fatalf("402 message: %s", msg)
	}

	// Revoked key → 401 → exit 3 with a login hint.
	t.Setenv(creds.EnvAPIKey, "ork_revoked_key_000000000000")
	fb.mu.Lock()
	fb.bundleLimit = false
	fb.mu.Unlock()
	if code := app.run(t, "sync"); code != ExitAuth || !strings.Contains(app.errOut.String(), "login") {
		t.Fatalf("401: %d %s", code, app.errOut)
	}
}

func TestWhoamiAndLogout(t *testing.T) {
	fb := newFakeBackend(t)
	app := newTestApp(t, fb, t.TempDir())
	app.login(t)
	if code := app.run(t, "whoami", "--json"); code != 0 {
		t.Fatal(app.errOut.String())
	}
	var who map[string]any
	if err := json.Unmarshal(app.out.Bytes(), &who); err != nil || who["valid"] != true || who["source"] != "file" {
		t.Fatalf("whoami: %v %s", err, app.out)
	}
	if code := app.run(t, "logout"); code != 0 {
		t.Fatal(app.errOut.String())
	}
	if len(fb.revoked) != 1 || fb.revoked[0] != "k1" {
		t.Fatalf("key not revoked: %v", fb.revoked)
	}
	if code := app.run(t, "whoami"); code != ExitAuth {
		t.Fatalf("whoami after logout: %d", code)
	}
}

func TestKeyringStorage(t *testing.T) {
	fb := newFakeBackend(t)
	app := newTestApp(t, fb, t.TempDir())
	kr := &memKeyring{m: map[string]string{}}
	app.Store.Keyring = kr
	app.login(t)
	if len(kr.m) != 1 {
		t.Fatalf("key not stored in keyring: %v", kr.m)
	}
	data, _ := os.ReadFile(app.Store.Path())
	if strings.Contains(string(data), testKey) {
		t.Fatal("API key leaked into credentials.json while a keyring is available")
	}
	cr, err := app.Store.Get(fb.srv.URL)
	if err != nil || cr.APIKey != testKey || cr.Source != "keyring" {
		t.Fatalf("get: %+v %v", cr, err)
	}
	t.Setenv(creds.EnvAPIKey, "ork_env_override_123456789")
	if cr, _ := app.Store.Get(fb.srv.URL); cr.Source != "env" {
		t.Fatal("env key must win")
	}
}

type memKeyring struct{ m map[string]string }

func (k *memKeyring) Set(s, u, p string) error { k.m[s+"|"+u] = p; return nil }
func (k *memKeyring) Get(s, u string) (string, error) {
	v, ok := k.m[s+"|"+u]
	if !ok {
		return "", os.ErrNotExist
	}
	return v, nil
}
func (k *memKeyring) Delete(s, u string) error { delete(k.m, s+"|"+u); return nil }

func TestInitInteractivePick(t *testing.T) {
	fb := newFakeBackend(t)
	repo := setupRepo(t)
	app := newTestApp(t, fb, repo)
	app.login(t)
	if code := app.run(t, "init"); code != ExitUsage {
		t.Fatalf("non-tty multi-project init should need --project: %d", code)
	}
	app.IsTTY = func() bool { return true }
	app.Stdin = strings.NewReader("2\n")
	if code := app.run(t, "init"); code != 0 {
		t.Fatal(app.errOut.String())
	}
	data, _ := os.ReadFile(filepath.Join(repo, ".orchestrai", "config.json"))
	if !strings.Contains(string(data), `"project_id": "p2"`) || strings.Contains(string(data), "ork_") {
		t.Fatalf("config: %s", data)
	}
	if code := app.run(t, "init", "--project", "p1"); code != ExitUsage {
		t.Fatal("rebinding without --force should fail")
	}
	if code := app.run(t, "init", "--project", "p1", "--force"); code != 0 {
		t.Fatal(app.errOut.String())
	}
}

func TestAddRemoveLibrary(t *testing.T) {
	fb := newFakeBackend(t)
	repo := setupRepo(t)
	app := newTestApp(t, fb, repo)
	app.login(t)
	app.run(t, "init", "--project", "p1")
	app.run(t, "sync")

	if code := app.run(t, "add", "acme/pr-review@1.2.0"); code != 0 {
		t.Fatalf("add: %s %s", app.out, app.errOut)
	}
	if !strings.Contains(app.out.String(), "Installed acme/pr-review@1.2.0 (skill)") {
		t.Fatalf("add output: %s", app.out)
	}
	skill := filepath.Join(repo, ".claude", "skills", "pr-review", "SKILL.md")
	if _, err := os.Stat(skill); err != nil {
		t.Fatal("installed skill not synced")
	}
	if code := app.run(t, "library", "list", "--json"); code != 0 || !strings.Contains(app.out.String(), "pr-review") {
		t.Fatalf("library list: %d %s %s", code, app.out, app.errOut)
	}
	if code := app.run(t, "library", "show", "acme/pr-review"); code != 0 || !strings.Contains(app.out.String(), "Install: orchestrai add acme/pr-review") {
		t.Fatalf("show: %s %s", app.out, app.errOut)
	}
	if code := app.run(t, "remove", "acme/pr-review"); code != 0 {
		t.Fatalf("remove: %s", app.errOut)
	}
	if _, err := os.Stat(skill); !os.IsNotExist(err) {
		t.Fatal("removed skill still on disk")
	}
}

func TestLibraryPublish(t *testing.T) {
	fb := newFakeBackend(t)
	repo := setupRepo(t)
	app := newTestApp(t, fb, repo)
	app.login(t)
	app.run(t, "init", "--project", "p1")
	skillDir := filepath.Join(repo, "skills", "deploy-checklist")
	_ = os.MkdirAll(skillDir, 0o755)
	_ = os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: Deploy Checklist\ndescription: Steps before deploying\n---\nCheck things.\n"), 0o644)

	if code := app.run(t, "library", "publish", skillDir, "--scope", "community"); code != 0 {
		t.Fatalf("publish: %s %s", app.out, app.errOut)
	}
	if len(fb.published) != 1 || fb.published[0]["kind"] != "skill" || fb.published[0]["scope"] != "community" || fb.published[0]["slug"] != "deploy-checklist" {
		t.Fatalf("published = %v", fb.published)
	}
	if !strings.Contains(app.out.String(), "npx @orchestrai/cli add ada/deploy-checklist") {
		t.Fatalf("publish output: %s", app.out)
	}
	_ = os.WriteFile(filepath.Join(skillDir, "env.sh"), []byte("export STRIPE_KEY=sk_live_"+strings.Repeat("a1", 14)+"\n"), 0o644)
	if code := app.run(t, "library", "publish", skillDir); code != ExitRefused || !strings.Contains(app.errOut.String(), "Stripe secret key") {
		t.Fatalf("secret publish: %d %s", code, app.errOut)
	}
	if len(fb.published) != 1 {
		t.Fatal("secret-bearing item was uploaded")
	}
	if code := app.run(t, "library", "publish", skillDir, "--scope", "org"); code != ExitUsage && code != ExitRefused {
		t.Fatalf("org scope without org id: %d", code)
	}
}

func TestWorkflowsAndRooms(t *testing.T) {
	fb := newFakeBackend(t)
	repo := setupRepo(t)
	app := newTestApp(t, fb, repo)
	app.login(t)
	app.run(t, "init", "--project", "p1")

	if code := app.run(t, "workflows", "list"); code != 0 || !strings.Contains(app.out.String(), "Nightly backup") {
		t.Fatalf("list: %s %s", app.out, app.errOut)
	}
	if code := app.run(t, "workflows", "run", "nightly backup", "--input", "target=s3", "--input", "retries:=3", "--wait"); code != 0 {
		t.Fatalf("run: %d %s %s", code, app.out, app.errOut)
	}
	if !strings.Contains(app.out.String(), "Execution ex-1 completed.") {
		t.Fatalf("run output: %s", app.out)
	}
	if fb.workflowInput["target"] != "s3" || fb.workflowInput["retries"] != float64(3) {
		t.Fatalf("inputs = %#v", fb.workflowInput)
	}
	if code := app.run(t, "workflows", "run", "missing"); code == 0 {
		t.Fatal("unknown workflow accepted")
	}

	if code := app.run(t, "rooms", "list"); code != 0 || !strings.Contains(app.out.String(), "room-1") {
		t.Fatalf("rooms list: %s", app.out)
	}
	if code := app.run(t, "rooms", "open", "room-1", "--editor", "cursor"); code != 0 {
		t.Fatal(app.errOut.String())
	}
	if len(app.opened) != 1 || app.opened[0] != "cursor://orchestrai.orchestrai-live/room?project=p1&room=room-1" {
		t.Fatalf("opened = %v", app.opened)
	}
	if code := app.run(t, "rooms", "open", "room-1", "--editor", "browser", "--print"); code != 0 || strings.TrimSpace(app.out.String()) != "https://app.test/projects/p1/rooms/room-1" {
		t.Fatalf("print: %s", app.out)
	}
}

func TestAskStreamsReply(t *testing.T) {
	fb := newFakeBackend(t)
	repo := setupRepo(t)
	app := newTestApp(t, fb, repo)
	app.login(t)
	app.run(t, "init", "--project", "p1")
	if code := app.run(t, "ask", "how", "do", "I", "deploy?"); code != 0 {
		t.Fatalf("ask: %d %s %s", code, app.out, app.errOut)
	}
	if strings.TrimSpace(app.out.String()) != "Hello team!" {
		t.Fatalf("reply = %q", app.out.String())
	}
	if len(fb.messages) != 2 || fb.messages[0]["role"] != "user" || fb.messages[1]["content"] != "Hello team!" {
		t.Fatalf("persisted messages = %v", fb.messages)
	}

	fb.mu.Lock()
	fb.chatLimit = true
	fb.mu.Unlock()
	if code := app.run(t, "ask", "--conversation", "conv-1", "again"); code != ExitLimit {
		t.Fatalf("402 ask: %d", code)
	}
	if !strings.Contains(app.errOut.String(), "Buy credits to continue: https://app.test/settings/billing?from=live_cli_ask") {
		t.Fatalf("402 message: %s", app.errOut)
	}
}

func TestHelpAndUsage(t *testing.T) {
	fb := newFakeBackend(t)
	app := newTestApp(t, fb, t.TempDir())
	if code := app.run(t, "help"); code != 0 || !strings.Contains(app.out.String(), "Exit codes") {
		t.Fatal("help")
	}
	if code := app.run(t, "bogus"); code != ExitUsage {
		t.Fatal("unknown command")
	}
	if code := app.run(t, "sync", "--check", "--dry-run"); code != ExitUsage {
		t.Fatal("conflicting flags")
	}
	if code := app.run(t, "sync", "--help"); code != 0 {
		t.Fatal("--help")
	}
}

func TestEnvKeyIsBoundToTrustedHosts(t *testing.T) {
	t.Setenv("ORCHESTRAI_API_URL", "")
	if !envKeyAllowedFor(api.DefaultAPIURL) {
		t.Fatal("env key must work against the default API")
	}
	if envKeyAllowedFor("https://evil.example") {
		t.Fatal("env key must not be sent to a host from repo config")
	}
	t.Setenv("ORCHESTRAI_API_URL", "https://api.example.com/")
	if !envKeyAllowedFor("https://api.example.com") {
		t.Fatal("env key must work against ORCHESTRAI_API_URL")
	}
}
