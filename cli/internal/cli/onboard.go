package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/OrchestrAIio/orchestrai/cli/internal/api"
	"github.com/OrchestrAIio/orchestrai/cli/internal/creds"
	"github.com/OrchestrAIio/orchestrai/cli/internal/project"
)

// EnvInvokedAs is set by the npm launcher to how the user ran the CLI
// ("npx @orchestrai/cli" under npx), so every hint names a command that works.
const EnvInvokedAs = "ORCHESTRAI_CLI_INVOKED_AS"

// NpxProg is the program name printed when the CLI runs through npx.
const NpxProg = "npx @orchestrai/cli"

// progName returns the program name for hints: the launcher's value when it
// is one we know, otherwise ork or orchestrai from argv[0].
func progName(argv0, invokedAs string) string {
	switch invokedAs {
	case "orchestrai", "ork", NpxProg:
		return invokedAs
	}
	if strings.TrimSuffix(filepath.Base(argv0), ".exe") == "ork" {
		return "ork"
	}
	return "orchestrai"
}

// interactive reports whether the CLI may prompt: only when stdin is a TTY.
func (a *App) interactive() bool { return a.IsTTY != nil && a.IsTTY() }

// input is the one buffered reader over Stdin, shared by every prompt.
func (a *App) input() *bufio.Reader {
	if a.in == nil {
		a.in = bufio.NewReader(a.Stdin)
	}
	return a.in
}

// ask prints question on Stderr and reads one trimmed line.
func (a *App) ask(question string) (string, error) {
	fmt.Fprint(a.Stderr, question)
	line, err := a.input().ReadString('\n')
	line = strings.TrimSpace(line)
	switch {
	case err == nil:
		return line, nil
	case errors.Is(err, io.EOF) && line != "":
		return line, nil
	case errors.Is(err, io.EOF):
		return "", usagef("no answer (input closed)")
	}
	return "", err
}

// confirm asks a yes/no question; an empty answer is yes.
func (a *App) confirm(question string) (bool, error) {
	for attempt := 0; attempt < 3; attempt++ {
		ans, err := a.ask(question + " [Y/n] ")
		if err != nil {
			return false, err
		}
		switch strings.ToLower(ans) {
		case "", "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		fmt.Fprintln(a.Stderr, "Answer y or n.")
	}
	return false, usagef("no answer")
}

// shellQuote quotes one argument for a POSIX shell when it needs it.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%+=:,./_-", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// command renders "<prog> <args…>" as a copyable command line.
func (a *App) command(args ...string) string {
	parts := []string{a.Prog}
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

// rerun is the command line the user ran, for "then run … again" hints.
func (a *App) rerun() string { return a.command(a.args...) }

// subcommand is the command being run ("init", "add", …).
func (a *App) subcommand() string {
	if len(a.args) == 0 {
		return ""
	}
	return a.args[0]
}

// thenRerun appends the user's own command line to steps unless the last
// step already is that command (init's own dead ends).
func (a *App) thenRerun(steps ...string) []string {
	if a.subcommand() == "init" || len(a.args) == 0 {
		return steps
	}
	return append(steps, a.rerun())
}

// nextSteps is an error whose message is a numbered list of commands.
func nextSteps(code int, headline string, steps ...string) error {
	var b strings.Builder
	b.WriteString(headline)
	if len(steps) == 1 {
		fmt.Fprintf(&b, "\nRun: %s", steps[0])
	} else {
		b.WriteString("\nRun:")
		for i, s := range steps {
			fmt.Fprintf(&b, "\n  %d. %s", i+1, s)
		}
	}
	return &exitError{code: code, err: errors.New(b.String())}
}

// onboardOpts carries the flags of a command that may sign in, create a team
// space and bind the repository on the way (init, add).
type onboardOpts struct {
	projectID   string // --project
	createSpace string // --create-space
	yes         bool   // --yes: never prompt; take the deterministic choice
}

// canChoose reports whether a team space can be chosen without --project:
// by prompting (TTY) or deterministically (--yes / --create-space).
func (o onboardOpts) canChoose(a *App) bool {
	return a.interactive() || o.yes || o.createSpace != ""
}

// signedInClient returns an authenticated client. When no key is stored it
// runs the device flow inline on a TTY; otherwise it returns the exact
// commands to run (sign in, bind when needed, then the same command again).
func (a *App) signedInClient(ctx context.Context, u string, needsBinding bool) (*api.Client, error) {
	c, _, err := a.authedClient(u)
	if err == nil || !errors.Is(err, creds.ErrNotLoggedIn) {
		return c, err
	}
	if !a.interactive() {
		steps := []string{a.command("login")}
		if needsBinding && a.subcommand() != "init" {
			steps = append(steps, a.command("init"))
		}
		steps = append(steps, a.rerun())
		return nil, nextSteps(ExitAuth, "You are not signed in to OrchestrAI.", steps...)
	}
	fmt.Fprintln(a.Stderr, "You are not signed in to OrchestrAI yet. Signing in first.")
	if err := a.deviceLogin(ctx, u, "", false); err != nil {
		return nil, err
	}
	c, _, err = a.authedClient(u)
	return c, err
}

// defaultSpaceName names a new team space after the repository folder.
func defaultSpaceName(root string) string {
	name := strings.TrimSpace(filepath.Base(root))
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "My team"
	}
	if r := []rune(name); len(r) > 120 {
		name = string(r[:120])
	}
	return name
}

// createSpace creates a team space and says so.
func (a *App) createSpace(ctx context.Context, c *api.Client, name string) (*api.Project, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, usagef("the team space name must not be empty")
	}
	p, err := c.CreateLiveSpace(ctx, name)
	if err != nil {
		return nil, err
	}
	if p.Name == "" {
		p.Name = name
	}
	fmt.Fprintf(a.Stderr, "Created team space %s (%s).\n", p.Name, p.ID)
	return p, nil
}

// chooseSpace resolves the team space to bind: --project, --create-space, the
// only one, a prompt (TTY), or with --yes a new one when there is none. Every
// non-interactive dead end names the exact command to run.
func (a *App) chooseSpace(ctx context.Context, c *api.Client, rc *repoContext, o onboardOpts) (*api.Project, error) {
	if o.projectID != "" {
		p, err := c.GetProject(ctx, o.projectID)
		switch {
		case err == nil:
			return p, nil
		case api.IsStatus(err, http.StatusNotFound):
			// GET /projects/:id only resolves projects you own; members of a
			// team space cannot look it up but can still sync it.
			fmt.Fprintf(a.Stderr, "Note: could not look up project %s (you may be a member rather than the owner); binding anyway.\n", o.projectID)
			return &api.Project{ID: o.projectID}, nil
		default:
			return nil, err
		}
	}
	if o.createSpace != "" {
		return a.createSpace(ctx, c, o.createSpace)
	}
	projects, err := c.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	suggested := defaultSpaceName(rc.Root)
	switch {
	case len(projects) == 1:
		return &projects[0], nil
	case len(projects) == 0 && o.yes:
		return a.createSpace(ctx, c, suggested)
	case len(projects) == 0 && a.interactive():
		ok, err := a.confirm("You have no team spaces yet. Create one now?")
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nextSteps(ExitNotBound, "No team space was created. To create one later:",
				a.thenRerun(a.command("init", "--create-space", suggested))...)
		}
		name, err := a.ask(fmt.Sprintf("Team space name [%s]: ", suggested))
		if err != nil {
			return nil, err
		}
		if name == "" {
			name = suggested
		}
		return a.createSpace(ctx, c, name)
	case len(projects) == 0:
		return nil, nextSteps(ExitNotBound, "You have no team spaces yet. Create one and bind this repository:",
			a.thenRerun(a.command("init", "--create-space", suggested))...)
	case a.interactive() && !o.yes:
		return a.pickProject(ctx, c, projects, suggested)
	}
	var b strings.Builder
	b.WriteString("You belong to several team spaces; choose one with --project:")
	for _, p := range projects {
		fmt.Fprintf(&b, "\n  %s  %s", p.ID, p.Name)
	}
	return nil, nextSteps(ExitUsage, b.String(), a.thenRerun(a.command("init", "--project", "<id>"))...)
}

// pickProject asks which team space to use; "n" creates a new one.
func (a *App) pickProject(ctx context.Context, c *api.Client, projects []api.Project, suggested string) (*api.Project, error) {
	fmt.Fprintln(a.Stderr, "Choose a team space:")
	for i, p := range projects {
		fmt.Fprintf(a.Stderr, "  %2d) %s  %s\n", i+1, p.Name, p.ID)
	}
	fmt.Fprintln(a.Stderr, "   n) Create a new team space")
	for attempt := 0; attempt < 3; attempt++ {
		line, err := a.ask(fmt.Sprintf("Number [1-%d] or n: ", len(projects)))
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(line, "n") {
			name, err := a.ask(fmt.Sprintf("Team space name [%s]: ", suggested))
			if err != nil {
				return nil, err
			}
			if name == "" {
				name = suggested
			}
			return a.createSpace(ctx, c, name)
		}
		if n, cerr := strconv.Atoi(line); cerr == nil && n >= 1 && n <= len(projects) {
			return &projects[n-1], nil
		}
		for i := range projects {
			if line != "" && (projects[i].ID == line || strings.EqualFold(projects[i].Name, line)) {
				return &projects[i], nil
			}
		}
		fmt.Fprintln(a.Stderr, "Not a valid choice.")
	}
	return nil, usagef("no team space selected")
}

// bindRepo writes .orchestrai/config.json for chosen and reloads the binding.
func (a *App) bindRepo(rc *repoContext, chosen *api.Project, u string, tools []string, force bool) error {
	if rc.Bound && rc.Cfg.ProjectID != chosen.ID && !force {
		return usagef("%s is already bound to project %s; pass --force to rebind", rc.Root, rc.Cfg.ProjectID)
	}
	cfg := &project.Config{ProjectID: chosen.ID, APIURL: u, Tools: tools}
	if rc.Cfg != nil {
		cfg.AppURL, cfg.OrgID = rc.Cfg.AppURL, rc.Cfg.OrgID
		if tools == nil {
			cfg.Tools = rc.Cfg.Tools
		}
	}
	if err := project.Save(rc.Root, cfg); err != nil {
		return err
	}
	rc.Bound, rc.Cfg = true, cfg
	label := chosen.ID
	if chosen.Name != "" {
		label = fmt.Sprintf("%s (%s)", chosen.Name, chosen.ID)
	}
	fmt.Fprintf(a.Stdout, "Bound %s to %s.\nWrote %s (commit it so teammates share the binding).\n",
		rc.Root, label, project.ConfigPath(rc.Root))
	return nil
}

// onboardSession is projectSession for commands that chain the first-run
// steps (add): it signs in when needed and, when no team space is named by
// --project, ORCHESTRAI_PROJECT_ID or the repository binding, chooses or
// creates one and binds the repository before the command continues.
func (a *App) onboardSession(ctx context.Context, apiFlag, projectFlag string, o onboardOpts) (*session, error) {
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
	if pid == "" && !o.canChoose(a) {
		// Say everything that is missing at once, in order.
		if _, _, cerr := a.authedClient(u); errors.Is(cerr, creds.ErrNotLoggedIn) {
			return nil, nextSteps(ExitAuth, "You are not signed in to OrchestrAI and this repository is not bound to a team space.",
				a.command("login"), a.command("init"), a.rerun())
		}
		return nil, nextSteps(ExitNotBound, "This repository is not bound to an OrchestrAI team space.",
			a.command("init"), a.rerun())
	}
	c, err := a.signedInClient(ctx, u, pid == "" && !o.yes && o.createSpace == "")
	if err != nil {
		return nil, err
	}
	if pid == "" {
		chosen, err := a.chooseSpace(ctx, c, rc, o)
		if err != nil {
			return nil, err
		}
		if err := a.bindRepo(rc, chosen, u, nil, false); err != nil {
			return nil, err
		}
		pid = chosen.ID
	}
	return &session{repo: rc, apiURL: u, appURL: a.appURL(rc.Cfg), projectID: pid, client: c}, nil
}
