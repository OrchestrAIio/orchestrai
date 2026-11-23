package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OrchestrAIio/orchestrai/cli/internal/project"
)

func TestProgName(t *testing.T) {
	cases := []struct{ argv0, env, want string }{
		{"/usr/local/bin/orchestrai", "", "orchestrai"},
		{"/usr/local/bin/ork", "", "ork"},
		{"/usr/local/bin/ork.exe", "", "ork"},
		{"/tmp/orchestrai_1.0_linux_amd64", "", "orchestrai"},
		{"orchestrai", "npx @orchestrai/cli", "npx @orchestrai/cli"},
		{"ork", "orchestrai", "orchestrai"},
		// Unknown launcher values are ignored, never printed.
		{"ork", "rm -rf /", "ork"},
		{"orchestrai", "npx evil", "orchestrai"},
	}
	for _, c := range cases {
		if got := progName(c.argv0, c.env); got != c.want {
			t.Errorf("progName(%q, %q) = %q, want %q", c.argv0, c.env, got, c.want)
		}
	}
}

func TestShellQuoteAndCommand(t *testing.T) {
	app := &App{Prog: NpxProg}
	if got := app.command("add", "acme/pr-review@1.2.0", "--create-space", "My team"); got != "npx @orchestrai/cli add acme/pr-review@1.2.0 --create-space 'My team'" {
		t.Fatalf("command = %q", got)
	}
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Fatalf("quote = %q", got)
	}
	if got := shellQuote(""); got != "''" {
		t.Fatalf("empty = %q", got)
	}
}

func TestHintsUseTheInvokedProgram(t *testing.T) {
	if got := withProg("not signed in; run `orchestrai login`", NpxProg); got != "not signed in; run `npx @orchestrai/cli login`" {
		t.Fatalf("withProg = %q", got)
	}
	if got := withProg("run `orchestrai init`", "orchestrai"); got != "run `orchestrai init`" {
		t.Fatalf("withProg unchanged = %q", got)
	}
	fb := newFakeBackend(t)
	repo := setupRepo(t)
	app := newTestApp(t, fb, repo)
	app.Prog = NpxProg
	app.login(t)
	// Messages from the project package carry the default name; the CLI prints the real one.
	if code := app.run(t, "sync"); code != ExitNotBound {
		t.Fatalf("sync unbound exit %d", code)
	}
	if !strings.Contains(app.errOut.String(), "run `npx @orchestrai/cli init`") || strings.Contains(app.errOut.String(), "`orchestrai init`") {
		t.Fatalf("hint names the wrong program: %s", app.errOut)
	}
}

func TestLoginEndsWithNextStep(t *testing.T) {
	fb := newFakeBackend(t)
	repo := setupRepo(t)
	app := newTestApp(t, fb, repo)
	app.Prog = NpxProg
	app.login(t)
	if !strings.HasSuffix(app.out.String(), "Next: npx @orchestrai/cli init\n") {
		t.Fatalf("login output: %q", app.out)
	}
	if code := app.run(t, "init", "--project", "p1"); code != 0 {
		t.Fatalf("init: %d %s", code, app.errOut)
	}
	if !strings.HasSuffix(app.out.String(), "Next: npx @orchestrai/cli sync\n") {
		t.Fatalf("init output: %q", app.out)
	}
	if code := app.run(t, "login", "--no-browser"); code != 0 {
		t.Fatalf("login again: %d", code)
	}
	if !strings.HasSuffix(app.out.String(), "Next: npx @orchestrai/cli sync\n") {
		t.Fatalf("bound login output: %q", app.out)
	}
}

func readBinding(t *testing.T, repo string) string {
	t.Helper()
	data, err := os.ReadFile(project.ConfigPath(repo))
	if err != nil {
		return ""
	}
	var cfg project.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.ProjectID
}

func TestAddNonInteractiveNotSignedInPrintsEveryStep(t *testing.T) {
	fb := newFakeBackend(t)
	repo := setupRepo(t)
	app := newTestApp(t, fb, repo)
	app.Prog = NpxProg
	code := app.run(t, "add", "item-1")
	if code != ExitAuth {
		t.Fatalf("exit %d: %s", code, app.errOut)
	}
	want := "You are not signed in to OrchestrAI and this repository is not bound to a team space.\nRun:\n" +
		"  1. npx @orchestrai/cli login\n  2. npx @orchestrai/cli init\n  3. npx @orchestrai/cli add item-1\n"
	if !strings.Contains(app.errOut.String(), want) {
		t.Fatalf("stderr:\n%s", app.errOut)
	}
	if len(fb.deviceNames) != 0 || len(fb.installs) != 0 {
		t.Fatalf("non-interactive add must not start a sign-in or install")
	}
}

func TestAddNonInteractiveSignedInUnbound(t *testing.T) {
	fb := newFakeBackend(t)
	repo := setupRepo(t)
	app := newTestApp(t, fb, repo)
	app.login(t)
	if code := app.run(t, "add", "item-1"); code != ExitNotBound {
		t.Fatalf("exit %d: %s", code, app.errOut)
	}
	if !strings.Contains(app.errOut.String(), "1. orchestrai init\n  2. orchestrai add item-1") {
		t.Fatalf("stderr:\n%s", app.errOut)
	}
	if readBinding(t, repo) != "" || len(fb.installs) != 0 {
		t.Fatal("nothing may be bound or installed")
	}
}

func TestAddYesWithSeveralSpacesNeedsProject(t *testing.T) {
	fb := newFakeBackend(t)
	repo := setupRepo(t)
	app := newTestApp(t, fb, repo)
	app.login(t)
	if code := app.run(t, "add", "acme/pr-review", "--yes"); code != ExitUsage {
		t.Fatalf("exit %d: %s", code, app.errOut)
	}
	e := app.errOut.String()
	if !strings.Contains(e, "p1  Platform") || !strings.Contains(e, "p2  Data") ||
		!strings.Contains(e, "1. orchestrai init --project '<id>'") || !strings.Contains(e, "2. orchestrai add acme/pr-review --yes") {
		t.Fatalf("stderr:\n%s", e)
	}
	if readBinding(t, repo) != "" || len(fb.installs) != 0 {
		t.Fatal("nothing may be bound or installed")
	}
}

func TestAddYesBindsTheOnlySpaceAndInstalls(t *testing.T) {
	fb := newFakeBackend(t)
	fb.setProjects(map[string]any{"id": "p1", "name": "Platform"})
	repo := setupRepo(t)
	app := newTestApp(t, fb, repo)
	app.login(t)
	if code := app.run(t, "add", "acme/pr-review", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s%s", code, app.out, app.errOut)
	}
	if got := readBinding(t, repo); got != "p1" {
		t.Fatalf("binding = %q", got)
	}
	o := app.out.String()
	if !strings.Contains(o, "Bound "+repo+" to Platform (p1).") || !strings.Contains(o, "Installed acme/pr-review@1.2.0") {
		t.Fatalf("stdout:\n%s", o)
	}
	if _, err := os.Stat(filepath.Join(repo, ".claude/skills/pr-review/SKILL.md")); err != nil {
		t.Fatalf("sync did not write the skill: %v", err)
	}
}

func TestInitNonInteractiveWithoutSpaces(t *testing.T) {
	fb := newFakeBackend(t)
	fb.setProjects()
	repo := setupRepo(t)
	app := newTestApp(t, fb, repo)
	app.login(t)
	if code := app.run(t, "init"); code != ExitNotBound {
		t.Fatalf("exit %d: %s", code, app.errOut)
	}
	want := "You have no team spaces yet. Create one and bind this repository:\nRun: orchestrai init --create-space " + shellQuote(filepath.Base(repo)) + "\n"
	if !strings.Contains(app.errOut.String(), want) {
		t.Fatalf("stderr:\n%s", app.errOut)
	}
	if len(fb.createdSpaces) != 0 {
		t.Fatal("no space may be created without --yes or a prompt")
	}

	// The printed command works as is.
	if code := app.run(t, "init", "--create-space", filepath.Base(repo)); code != 0 {
		t.Fatalf("create-space exit %d: %s", code, app.errOut)
	}
	if got := readBinding(t, repo); got != "p-new-1" || fb.createdSpaces[0] != filepath.Base(repo) {
		t.Fatalf("binding %q, created %v", got, fb.createdSpaces)
	}
}

func TestInitYesCreatesASpaceWhenThereIsNone(t *testing.T) {
	fb := newFakeBackend(t)
	fb.setProjects()
	repo := setupRepo(t)
	app := newTestApp(t, fb, repo)
	app.login(t)
	if code := app.run(t, "init", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, app.errOut)
	}
	if len(fb.createdSpaces) != 1 || fb.createdSpaces[0] != filepath.Base(repo) || readBinding(t, repo) != "p-new-1" {
		t.Fatalf("created %v binding %q", fb.createdSpaces, readBinding(t, repo))
	}
}

func TestAddInteractiveChainsSignInCreateBindInstall(t *testing.T) {
	fb := newFakeBackend(t)
	fb.setProjects()
	repo := setupRepo(t)
	app := newTestApp(t, fb, repo)
	app.Prog = NpxProg
	app.IsTTY = func() bool { return true }
	app.Stdin = strings.NewReader("y\nAcme\n")
	code := app.run(t, "add", "item-1")
	if code != 0 {
		t.Fatalf("exit %d: %s%s", code, app.out, app.errOut)
	}
	e, o := app.errOut.String(), app.out.String()
	for _, s := range []string{"You are not signed in to OrchestrAI yet. Signing in first.", "WXYZ-1234", "You have no team spaces yet. Create one now? [Y/n]", "Created team space Acme (p-new-1)."} {
		if !strings.Contains(e, s) {
			t.Errorf("stderr lacks %q:\n%s", s, e)
		}
	}
	for _, s := range []string{"Signed in as Ada Lovelace", "Bound " + repo + " to Acme (p-new-1).", "Installed acme/pr-review@1.2.0"} {
		if !strings.Contains(o, s) {
			t.Errorf("stdout lacks %q:\n%s", s, o)
		}
	}
	if len(app.opened) != 1 || !strings.Contains(app.opened[0], "/live/connect?code=WXYZ-1234") {
		t.Fatalf("opened = %v", app.opened)
	}
	if readBinding(t, repo) != "p-new-1" || len(fb.installs) != 1 {
		t.Fatalf("binding %q installs %d", readBinding(t, repo), len(fb.installs))
	}
	if len(fb.deviceNames) != 1 || fb.deviceNames[0] == "" {
		t.Fatalf("device names = %v", fb.deviceNames)
	}
}

func TestInitInteractivePicksOrCreates(t *testing.T) {
	fb := newFakeBackend(t)
	repo := setupRepo(t)
	app := newTestApp(t, fb, repo)
	app.login(t)
	app.IsTTY = func() bool { return true }
	app.Stdin = strings.NewReader("2\n")
	if code := app.run(t, "init"); code != 0 {
		t.Fatalf("exit %d: %s", code, app.errOut)
	}
	if readBinding(t, repo) != "p2" {
		t.Fatalf("binding = %q", readBinding(t, repo))
	}
	if !strings.Contains(app.errOut.String(), "n) Create a new team space") {
		t.Fatalf("picker: %s", app.errOut)
	}

	app.in = nil
	app.Stdin = strings.NewReader("n\n\n")
	if code := app.run(t, "init", "--force"); code != 0 {
		t.Fatalf("exit %d: %s", code, app.errOut)
	}
	if readBinding(t, repo) != "p-new-1" || fb.createdSpaces[0] != filepath.Base(repo) {
		t.Fatalf("binding %q created %v", readBinding(t, repo), fb.createdSpaces)
	}
}

func TestInteractiveDeclineCreatingASpace(t *testing.T) {
	fb := newFakeBackend(t)
	fb.setProjects()
	repo := setupRepo(t)
	app := newTestApp(t, fb, repo)
	app.login(t)
	app.IsTTY = func() bool { return true }
	app.Stdin = strings.NewReader("n\n")
	if code := app.run(t, "add", "item-1"); code != ExitNotBound {
		t.Fatalf("exit %d: %s", code, app.errOut)
	}
	if !strings.Contains(app.errOut.String(), "orchestrai init --create-space") || !strings.Contains(app.errOut.String(), "orchestrai add item-1") {
		t.Fatalf("stderr: %s", app.errOut)
	}
	if len(fb.createdSpaces) != 0 || len(fb.installs) != 0 {
		t.Fatal("declining must create and install nothing")
	}
}

func TestNoPromptWithoutATTY(t *testing.T) {
	fb := newFakeBackend(t)
	repo := setupRepo(t)
	app := newTestApp(t, fb, repo)
	app.login(t)
	// Input is available but stdin is not a terminal: it must not be read.
	app.Stdin = strings.NewReader("1\n")
	if code := app.run(t, "init"); code != ExitUsage {
		t.Fatalf("exit %d: %s", code, app.errOut)
	}
	if strings.Contains(app.errOut.String(), "Number [") || readBinding(t, repo) != "" {
		t.Fatalf("prompted without a TTY: %s", app.errOut)
	}
}

func TestLimitValueUnits(t *testing.T) {
	cases := []struct{ code, n, want string }{
		{"LIVE_LIMIT_LIVE_ROOM_SECONDS_MONTH", "108000", "30 hours"},
		{"LIVE_LIMIT_LIVE_ROOM_SECONDS_MONTH", "3600", "1 hour"},
		{"LIVE_LIMIT_LIVE_SESSION_SECONDS", "1800", "30 minutes"},
		{"LIVE_LIMIT_LIVE_ROOM_BYTES_MONTH", "3221225472", "3 GiB"},
		{"LIVE_LIMIT_LIBRARY_ITEM_BYTES", "65536", "64 KB"},
		{"LIVE_LIMIT_LIBRARY_SPACE_BYTES", "5242880", "5 MiB"},
		{"LIVE_LIMIT_DIGEST_LOOKBACK_DAYS", "7", "7 days"},
		{"LIVE_LIMIT_LIVE_ROOMS", "1", "1"},
		{"LIVE_LIMIT_LIVE_ROOMS", "-1", "unlimited"},
		{"LIVE_LIMIT_LIVE_ROOMS", "", "-"},
	}
	for _, c := range cases {
		if got := limitValue(c.code, json.Number(c.n)); got != c.want {
			t.Errorf("limitValue(%s, %s) = %q, want %q", c.code, c.n, got, c.want)
		}
	}
}

func TestStableDeviceName(t *testing.T) {
	if got := stableDeviceName("  ada-laptop.local\n"); got != "ada-laptop.local" {
		t.Fatalf("got %q", got)
	}
	if got := stableDeviceName("bad\x07name"); got != "badname" {
		t.Fatalf("got %q", got)
	}
	if got := stableDeviceName(""); got != "this device" {
		t.Fatalf("got %q", got)
	}
	if got := stableDeviceName(strings.Repeat("é", 70)); len([]rune(got)) != 64 {
		t.Fatalf("got %d runes", len([]rune(got)))
	}
}
