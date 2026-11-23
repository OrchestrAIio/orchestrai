// Package cli implements the `orchestrai` (alias `ork`) command line.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/OrchestrAIio/orchestrai/cli/internal/api"
	"github.com/OrchestrAIio/orchestrai/cli/internal/creds"
	"github.com/OrchestrAIio/orchestrai/cli/internal/project"
)

// Version is set at build time with -ldflags "-X …/internal/cli.Version=x.y.z".
var Version = "dev"

// DeviceClient is the client id sent in the device flow and sync reports.
const DeviceClient = "ork"

// Env variables read by the CLI (credentials-related ones live in creds).
const (
	EnvProjectID = "ORCHESTRAI_PROJECT_ID"
	EnvAppURL    = "ORCHESTRAI_APP_URL"
	EnvWSURL     = "ORCHESTRAI_WS_URL"
)

// App holds the process environment so tests can drive commands in-process.
type App struct {
	Prog    string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Dir     string // working directory
	Store   *creds.Store
	HTTP    *http.Client
	OpenURL func(string) error
	Sleep   api.Sleeper
	Now     func() time.Time
	IsTTY   func() bool

	warnedRepoURL bool
	in            *bufio.Reader // shared reader over Stdin for prompts
	args          []string      // the command line after the program name
}

// Main is the process entry point; it returns the exit code.
func Main(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	argv0 := ""
	if len(argv) > 0 {
		argv0 = argv[0]
	}
	prog := progName(argv0, os.Getenv(EnvInvokedAs))
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", prog, err)
		return ExitFailure
	}
	store, err := creds.NewDefault()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", prog, err)
		return ExitFailure
	}
	app := &App{
		Prog: prog, Stdin: stdin, Stdout: stdout, Stderr: stderr, Dir: wd, Store: store,
		HTTP: &http.Client{Timeout: 60 * time.Second}, OpenURL: openBrowser, Sleep: api.RealSleep, Now: time.Now,
		IsTTY: func() bool { return isTerminal(os.Stdin) },
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var args []string
	if len(argv) > 1 {
		args = argv[1:]
	}
	return app.Run(ctx, args)
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

type command struct {
	name    string
	summary string
	run     func(a *App, ctx context.Context, args []string) error
}

func commands() []command {
	return []command{
		{"login", "Sign in from this machine (browser device flow)", (*App).cmdLogin},
		{"logout", "Remove stored credentials and revoke the API key", (*App).cmdLogout},
		{"whoami", "Show the signed-in account", (*App).cmdWhoami},
		{"init", "Bind this repository to a team space", (*App).cmdInit},
		{"sync", "Write the team's rules, skills and MCP servers into this repo", (*App).cmdSync},
		{"status", "Show drift between this repo and the team context", (*App).cmdStatus},
		{"diff", "Show the unified diff a sync would apply", (*App).cmdDiff},
		{"add", "Install a library item locally or into the team space", (*App).cmdAdd},
		{"remove", "Uninstall a library item from the team space, then sync", (*App).cmdRemove},
		{"library", "Browse, search and publish library items (list|search|show|publish)", (*App).cmdLibrary},
		{"check", "Scan this repository locally for infra & secret readiness issues (offline, free)", (*App).cmdCheck},
		{"audit", "Show read-only cloud cost savings and security audit findings", (*App).cmdAudit},
		{"workflows", "List and run OrchestrAI automations (list|run)", (*App).cmdWorkflows},
		{"ask", "Ask the team's OrchestrAI agent and stream the reply", (*App).cmdAsk},
		{"rooms", "List live co-coding rooms or open one (list|open)", (*App).cmdRooms},
		{"version", "Print the CLI version", (*App).cmdVersion},
	}
}

// Run dispatches args and returns the exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		if len(args) > 1 {
			return a.Run(ctx, []string{args[1], "--help"})
		}
		a.printHelp(a.Stdout)
		return ExitOK
	}
	if args[0] == "--version" || args[0] == "-v" {
		args[0] = "version"
	}
	for _, c := range commands() {
		if c.name == args[0] {
			a.args = append([]string(nil), args...)
			err := c.run(a, ctx, args[1:])
			if errors.Is(err, flag.ErrHelp) {
				return ExitOK
			}
			if err != nil {
				printError(a.Stderr, a.Prog, err, a.appURL(nil))
			}
			return exitCode(err)
		}
	}
	printError(a.Stderr, a.Prog, usagef("unknown command %q", args[0]), "")
	return ExitUsage
}

func (a *App) printHelp(w io.Writer) {
	fmt.Fprintf(w, "%s — the OrchestrAI CLI (alias: ork)\n\nUsage:\n  %s <command> [flags]\n\nCommands:\n", a.Prog, a.Prog)
	for _, c := range commands() {
		fmt.Fprintf(w, "  %-10s %s\n", c.name, c.summary)
	}
	fmt.Fprintf(w, `
Run '%s <command> --help' for the flags of one command.

Environment:
  ORCHESTRAI_API_KEY     API key (overrides stored credentials; use in CI)
  ORCHESTRAI_API_URL     API base URL (default %s)
  ORCHESTRAI_PROJECT_ID  Team space id (overrides .orchestrai/config.json)
  ORCHESTRAI_APP_URL     Web app URL (default %s)
  ORCHESTRAI_NO_KEYRING  Store the key in credentials.json instead of the OS keychain

Exit codes:
  0 ok · 1 out of sync / drift · 2 usage · 3 auth · 4 plan limit (402)
  5 API/network · 6 refused (unsafe path, malformed file, secrets) · 7 repo not bound
  8 other error · 9 remote run failed · 130 interrupted
`, a.Prog, api.DefaultAPIURL, api.DefaultAppURL)
}

func (a *App) cmdVersion(_ context.Context, args []string) error {
	fs := a.flags("version", "")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "%s %s (%s/%s)\n", a.Prog, Version, runtime.GOOS, runtime.GOARCH)
	return nil
}

// flags creates a FlagSet that prints usage to stderr.
func (a *App) flags(name, usage string) *flag.FlagSet {
	fs := flag.NewFlagSet(a.Prog+" "+name, flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(a.Stderr, "Usage: %s %s %s\n\nFlags:\n", a.Prog, name, usage)
		fs.PrintDefaults()
	}
	return fs
}

// parseFlags parses flags that may appear before or after positionals.
func parseFlags(fs *flag.FlagSet, args []string) error {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return err
			}
			return &usageError{msg: err.Error()}
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		if rest[0] == "--" {
			pos = append(pos, rest[1:]...)
			break
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
	return fs.Parse(append([]string{"--"}, pos...))
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// repoContext is the optional repository binding.
type repoContext struct {
	Root  string
	Bound bool
	Cfg   *project.Config
}

func (a *App) repo() (*repoContext, error) {
	root, bound, err := project.FindRoot(a.Dir)
	if err != nil {
		return nil, err
	}
	rc := &repoContext{Root: root, Bound: bound}
	if bound {
		cfg, err := project.Load(root)
		if err != nil {
			return nil, err
		}
		rc.Cfg = cfg
	}
	return rc, nil
}

// apiURL resolves: flag > ORCHESTRAI_API_URL > repo config > last login > default.
func (a *App) apiURL(flagVal string, cfg *project.Config) (string, error) {
	u := flagVal
	if u == "" {
		u = os.Getenv(creds.EnvAPIURL)
	}
	if u == "" && a.repoURLsTrusted(cfg) {
		u = cfg.APIURL
	}
	if u == "" && a.Store != nil {
		u = a.Store.DefaultAPIURL()
	}
	if u == "" {
		u = api.DefaultAPIURL
	}
	u = api.NormalizeURL(u)
	if err := api.ValidateBaseURL(u); err != nil {
		return "", &usageError{msg: err.Error()}
	}
	return u, nil
}

func (a *App) appURL(cfg *project.Config) string {
	if v := os.Getenv(EnvAppURL); v != "" {
		return api.NormalizeURL(v)
	}
	// A committed app_url only decides where links point, but links are
	// where people sign in, so a repo may only name the default app.
	if cfg != nil && cfg.AppURL != "" && api.NormalizeURL(cfg.AppURL) != api.NormalizeURL(api.DefaultAppURL) {
		fmt.Fprintf(a.Stderr, "warning: ignoring app_url %q from .orchestrai/config.json; set %s to use another web app\n", cfg.AppURL, EnvAppURL)
	}
	return api.DefaultAppURL
}

// repoURLsTrusted reports whether a repository's .orchestrai/config.json may
// choose the API/app host. The file is committed by anyone with push access,
// so a non-default host is honoured only when the user has explicitly signed
// in to it (`orchestrai login --api-url …`); otherwise it is ignored with a
// warning, so a cloned repo cannot redirect the user's key to another server.
func (a *App) repoURLsTrusted(cfg *project.Config) bool {
	if cfg == nil || cfg.APIURL == "" {
		return false
	}
	u := api.NormalizeURL(cfg.APIURL)
	if u == api.NormalizeURL(api.DefaultAPIURL) {
		return true
	}
	if a.Store != nil {
		if _, err := a.Store.GetStored(u); err == nil {
			return true
		}
	}
	if !a.warnedRepoURL {
		a.warnedRepoURL = true
		fmt.Fprintf(a.Stderr, "warning: ignoring api_url %q from .orchestrai/config.json (you have not signed in to that server; run `%s login --api-url %s` if you trust it)\n", u, a.Prog, u)
	}
	return false
}

func (a *App) newClient(apiURL, key string) *api.Client {
	c := api.New(apiURL, key, fmt.Sprintf("orchestrai-cli/%s (%s/%s)", Version, runtime.GOOS, runtime.GOARCH))
	if a.HTTP != nil {
		c.HTTP = a.HTTP
	}
	return c
}

// authedClient returns a client with stored or env credentials.
func (a *App) authedClient(apiURL string) (*api.Client, *creds.Credentials, error) {
	var cr *creds.Credentials
	var err error
	if envKeyAllowedFor(apiURL) {
		cr, err = a.Store.Get(apiURL)
	} else {
		// ORCHESTRAI_API_KEY is only ever sent to the default API or the
		// host named in ORCHESTRAI_API_URL, never to another host.
		cr, err = a.Store.GetStored(apiURL)
	}
	if err != nil {
		return nil, nil, err
	}
	return a.newClient(apiURL, cr.APIKey), cr, nil
}

// envKeyAllowedFor reports whether ORCHESTRAI_API_KEY may be sent to apiURL.
func envKeyAllowedFor(apiURL string) bool {
	u := api.NormalizeURL(apiURL)
	if u == api.NormalizeURL(api.DefaultAPIURL) {
		return true
	}
	env := os.Getenv(creds.EnvAPIURL)
	return env != "" && api.NormalizeURL(env) == u
}

// session bundles what project-scoped commands need.
type session struct {
	repo      *repoContext
	apiURL    string
	appURL    string
	projectID string
	client    *api.Client
}

// projectSession resolves repo, API URL, credentials and project id
// (flag > ORCHESTRAI_PROJECT_ID > .orchestrai/config.json).
func (a *App) projectSession(apiFlag, projectFlag string) (*session, error) {
	rc, err := a.repo()
	if err != nil {
		return nil, err
	}
	u, err := a.apiURL(apiFlag, rc.Cfg)
	if err != nil {
		return nil, err
	}
	pid := projectFlag
	if pid == "" {
		pid = os.Getenv(EnvProjectID)
	}
	if pid == "" && rc.Cfg != nil {
		pid = rc.Cfg.ProjectID
	}
	if pid == "" {
		return nil, project.ErrNotInitialized
	}
	c, _, err := a.authedClient(u)
	if err != nil {
		return nil, err
	}
	return &session{repo: rc, apiURL: u, appURL: a.appURL(rc.Cfg), projectID: pid, client: c}, nil
}

func (a *App) printJSON(v any) error {
	enc := json.NewEncoder(a.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

var toolNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// parseTools splits a --tools value.
func parseTools(v string) ([]string, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	var out []string
	for _, t := range strings.Split(v, ",") {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if !toolNameRe.MatchString(t) {
			return nil, usagef("invalid tool name %q in --tools", t)
		}
		out = append(out, t)
	}
	return out, nil
}

var editorSchemes = map[string]bool{"vscode": true, "vscode-insiders": true, "cursor": true, "windsurf": true, "vscodium": true}

// openBrowser opens http(s) URLs and editor deep links with the OS handler.
func openBrowser(u string) error {
	scheme, _, ok := strings.Cut(u, "://")
	if !ok || (scheme != "http" && scheme != "https" && !editorSchemes[scheme]) {
		return fmt.Errorf("refusing to open %q", u)
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		opener := "xdg-open"
		if _, err := exec.LookPath("wslview"); err == nil {
			opener = "wslview"
		}
		cmd = exec.Command(opener, u)
	}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
