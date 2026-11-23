package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/OrchestrAIio/orchestrai/cli/internal/api"
	"github.com/OrchestrAIio/orchestrai/cli/internal/project"
	"github.com/OrchestrAIio/orchestrai/cli/internal/syncer"
)

type syncMode int

const (
	modeApply syncMode = iota
	modeCheck
	modeDryRun
)

type syncOpts struct {
	mode  syncMode
	tools []string
}

// syncResult is the --json shape for sync and sync --check.
type syncResult struct {
	ProjectID     string           `json:"project_id"`
	BundleVersion string           `json:"bundle_version"`
	BundleHash    string           `json:"bundle_hash"`
	Mode          string           `json:"mode"` // apply | check | dry-run
	InSync        bool             `json:"in_sync"`
	NotModified   bool             `json:"not_modified"`
	Changes       []syncer.Change  `json:"changes"`
	Kept          []syncer.Kept    `json:"kept"`
	Warnings      []string         `json:"warnings"`
	Tools         []string         `json:"tools,omitempty"`
	Drift         []api.DriftEntry `json:"-"`
	plan          *syncer.Plan
}

func modeName(m syncMode) string {
	switch m {
	case modeCheck:
		return "check"
	case modeDryRun:
		return "dry-run"
	}
	return "apply"
}

// runSync fetches the bundle and checks, previews or applies it.
func (a *App) runSync(ctx context.Context, s *session, opts syncOpts) (*syncResult, error) {
	root := s.repo.Root
	prev, prevExists, err := syncer.LoadManifest(root)
	if err != nil {
		return nil, err
	}
	res := &syncResult{ProjectID: s.projectID, Mode: modeName(opts.mode), Changes: []syncer.Change{}, Kept: []syncer.Kept{}, Warnings: []string{}, Tools: opts.tools}

	// Only ask for a 304 when the working tree still matches the manifest
	// for the same tool selection; otherwise the full bundle is needed to
	// repair drift.
	etag := ""
	if prevExists && prev.ETag != "" && prev.SameTools(opts.tools) {
		if states, err := syncer.LocalStatus(root, prev); err == nil && syncer.Clean(states) {
			etag = prev.ETag
		}
	}
	body, newETag, err := s.client.FetchBundle(ctx, s.projectID, etag)
	if errors.Is(err, api.ErrNotModified) {
		res.NotModified, res.InSync = true, true
		res.BundleVersion, res.BundleHash = prev.BundleVersion, prev.BundleHash
		if opts.mode == modeApply {
			a.postReport(ctx, s, prev, opts.tools, nil)
		}
		return res, nil
	}
	if err != nil {
		return nil, err
	}
	bundle, err := syncer.ParseBundle(body)
	if err != nil {
		return nil, err
	}
	plan, err := syncer.BuildPlan(root, bundle, prev, syncer.Options{Tools: opts.tools})
	if err != nil {
		return nil, err
	}
	plan.Manifest.ETag = newETag
	res.plan = plan
	res.BundleVersion, res.BundleHash = string(bundle.Version), bundle.Hash
	res.Changes = append(res.Changes, plan.Pending()...)
	res.Kept = append(res.Kept, plan.Kept...)
	res.Warnings = append(res.Warnings, bundle.Warnings...)
	res.Warnings = append(res.Warnings, plan.Warnings...)
	res.InSync = !plan.OutOfSync(prev, prevExists)
	for _, c := range plan.Pending() {
		if c.LocalEdits {
			res.Drift = append(res.Drift, api.DriftEntry{Path: c.Path, Tool: c.Tool, State: "overwritten"})
		}
	}
	for _, k := range plan.Kept {
		res.Drift = append(res.Drift, api.DriftEntry{Path: k.Path, Tool: k.Tool, State: "kept_modified"})
	}
	if opts.mode != modeApply {
		return res, nil
	}
	if err := syncer.Apply(root, plan); err != nil {
		return nil, err
	}
	a.postReport(ctx, s, plan.Manifest, opts.tools, res.Drift)
	return res, nil
}

// postReport sends the sync report; failures only produce a warning.
func (a *App) postReport(ctx context.Context, s *session, m *syncer.Manifest, tools []string, drift []api.DriftEntry) {
	targets := tools
	if len(targets) == 0 {
		seen := map[string]bool{}
		for _, e := range m.Entries {
			if e.Tool != "" && !seen[e.Tool] {
				seen[e.Tool] = true
				targets = append(targets, e.Tool)
			}
		}
		sort.Strings(targets)
	}
	err := s.client.PostSyncReport(ctx, s.projectID, api.SyncReport{
		Client: DeviceClient, ClientVersion: Version, Repo: project.RepoIdentity(s.repo.Root),
		ToolTargets: targets, ManifestHash: m.Hash(), Drift: drift,
	})
	if err != nil {
		fmt.Fprintf(a.Stderr, "Warning: could not send the sync report (%v)\n", err)
	}
}

func (a *App) printSyncSummary(w io.Writer, res *syncResult) {
	for _, warn := range res.Warnings {
		fmt.Fprintf(a.Stderr, "warning: %s\n", warn)
	}
	if res.NotModified {
		fmt.Fprintf(w, "Already up to date (bundle %s).\n", orDash(res.BundleVersion))
		return
	}
	counts := map[syncer.Action]int{}
	for _, c := range res.Changes {
		counts[c.Action]++
		verb := string(c.Action)
		if res.Mode != "apply" {
			verb = "would " + verb
		}
		tool := ""
		if c.Tool != "" {
			tool = " (" + c.Tool + ")"
		}
		fmt.Fprintf(w, "  %-16s %s%s\n", verb, c.Path, tool)
	}
	for _, k := range res.Kept {
		fmt.Fprintf(w, "  %-16s %s (edited locally; no longer in the bundle)\n", "kept", k.Path)
	}
	switch res.Mode {
	case "check":
		if res.InSync {
			fmt.Fprintf(w, "In sync with bundle %s.\n", orDash(res.BundleVersion))
		} else {
			fmt.Fprintf(w, "Out of sync with bundle %s: %d to create, %d to update, %d to remove. Run `%s sync`.\n",
				orDash(res.BundleVersion), counts[syncer.ActCreate], counts[syncer.ActUpdate], counts[syncer.ActDelete], a.Prog)
		}
	case "dry-run":
		if len(res.Changes) == 0 {
			fmt.Fprintf(w, "Nothing to change (bundle %s).\n", orDash(res.BundleVersion))
		} else {
			fmt.Fprintf(w, "Dry run: %d file(s) would change. Run `%s diff` to see the content.\n", len(res.Changes), a.Prog)
		}
	default:
		if len(res.Changes) == 0 {
			fmt.Fprintf(w, "Up to date with bundle %s.\n", orDash(res.BundleVersion))
		} else {
			fmt.Fprintf(w, "Synced bundle %s: %d created, %d updated, %d removed.\n",
				orDash(res.BundleVersion), counts[syncer.ActCreate], counts[syncer.ActUpdate], counts[syncer.ActDelete])
		}
	}
}

// toolsFor picks --tools, else the repo config default.
func toolsFor(flagVal string, s *session) ([]string, error) {
	t, err := parseTools(flagVal)
	if err != nil || t != nil {
		return t, err
	}
	if s.repo.Cfg != nil {
		return parseTools(strings.Join(s.repo.Cfg.Tools, ","))
	}
	return nil, nil
}

func (a *App) cmdSync(ctx context.Context, args []string) error {
	fs := a.flags("sync", "[--check] [--dry-run] [--tools claude,cursor,vscode,codex,copilot] [flags]")
	check := fs.Bool("check", false, "exit 1 if the repository is out of sync (CI); writes nothing")
	dry := fs.Bool("dry-run", false, "print what would change; writes nothing")
	toolsFlag := fs.String("tools", "", "comma-separated tool targets (default: config tools, else all)")
	asJSON := fs.Bool("json", false, "print JSON")
	projectFlag := fs.String("project", "", "team space id (default: .orchestrai/config.json)")
	apiFlag := fs.String("api-url", "", "API base URL")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("sync takes no arguments")
	}
	if *check && *dry {
		return usagef("--check and --dry-run are mutually exclusive")
	}
	s, err := a.projectSession(*apiFlag, *projectFlag)
	if err != nil {
		return err
	}
	tools, err := toolsFor(*toolsFlag, s)
	if err != nil {
		return err
	}
	mode := modeApply
	if *check {
		mode = modeCheck
	} else if *dry {
		mode = modeDryRun
	}
	res, err := a.runSync(ctx, s, syncOpts{mode: mode, tools: tools})
	if err != nil {
		return err
	}
	if *asJSON {
		if err := a.printJSON(res); err != nil {
			return err
		}
	} else {
		a.printSyncSummary(a.Stdout, res)
	}
	if mode == modeCheck && !res.InSync {
		return silentExit(ExitOutOfSync)
	}
	return nil
}

// statusResult is the --json shape for status.
type statusResult struct {
	ProjectID     string             `json:"project_id"`
	Synced        bool               `json:"synced"`
	BundleVersion string             `json:"bundle_version"`
	BundleHash    string             `json:"bundle_hash"`
	Remote        *remoteState       `json:"remote,omitempty"`
	Files         []syncer.FileState `json:"files"`
	Drift         bool               `json:"drift"`
	Stale         bool               `json:"stale"`
}

type remoteState struct {
	Checked       bool   `json:"checked"`
	BundleVersion string `json:"bundle_version,omitempty"`
	BundleHash    string `json:"bundle_hash,omitempty"`
	Error         string `json:"error,omitempty"`
}

func (a *App) cmdStatus(ctx context.Context, args []string) error {
	fs := a.flags("status", "[flags]")
	asJSON := fs.Bool("json", false, "print JSON")
	offline := fs.Bool("offline", false, "only check local drift; do not contact the API")
	exitCodeFlag := fs.Bool("exit-code", false, "exit 1 when there is drift or the bundle is stale")
	projectFlag := fs.String("project", "", "team space id")
	apiFlag := fs.String("api-url", "", "API base URL")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	var s *session
	var err error
	if *offline {
		rc, rerr := a.repo()
		if rerr != nil {
			return rerr
		}
		if !rc.Bound && *projectFlag == "" {
			return project.ErrNotInitialized
		}
		s = &session{repo: rc, projectID: *projectFlag}
		if s.projectID == "" {
			s.projectID = rc.Cfg.ProjectID
		}
	} else if s, err = a.projectSession(*apiFlag, *projectFlag); err != nil {
		return err
	}
	m, exists, err := syncer.LoadManifest(s.repo.Root)
	if err != nil {
		return err
	}
	res := &statusResult{ProjectID: s.projectID, Synced: exists, BundleVersion: m.BundleVersion, BundleHash: m.BundleHash, Files: []syncer.FileState{}}
	if exists {
		states, err := syncer.LocalStatus(s.repo.Root, m)
		if err != nil {
			return err
		}
		res.Files = states
		res.Drift = !syncer.Clean(states)
	}
	if !*offline {
		rs := &remoteState{}
		etag := ""
		if exists {
			etag = m.ETag
		}
		body, _, ferr := s.client.FetchBundle(ctx, s.projectID, etag)
		switch {
		case errors.Is(ferr, api.ErrNotModified):
			rs.Checked = true
			rs.BundleVersion, rs.BundleHash = m.BundleVersion, m.BundleHash
		case ferr != nil:
			var le *api.LimitError
			if errors.As(ferr, &le) || exitCode(ferr) == ExitAuth {
				return ferr
			}
			rs.Error = ferr.Error()
		default:
			b, perr := syncer.ParseBundle(body)
			if perr != nil {
				rs.Error = perr.Error()
			} else {
				rs.Checked = true
				rs.BundleVersion, rs.BundleHash = string(b.Version), b.Hash
				res.Stale = !exists || b.Hash != m.BundleHash
			}
		}
		res.Remote = rs
	}
	if *asJSON {
		if err := a.printJSON(res); err != nil {
			return err
		}
	} else {
		a.printStatus(res)
	}
	if *exitCodeFlag && (res.Drift || res.Stale) {
		return silentExit(ExitOutOfSync)
	}
	return nil
}

func (a *App) printStatus(res *statusResult) {
	w := a.Stdout
	fmt.Fprintf(w, "Project: %s\n", res.ProjectID)
	if !res.Synced {
		fmt.Fprintf(w, "Never synced in this repository. Run `%s sync`.\n", a.Prog)
	} else {
		fmt.Fprintf(w, "Local bundle: %s (%s)\n", orDash(res.BundleVersion), short(res.BundleHash))
	}
	if r := res.Remote; r != nil {
		switch {
		case r.Error != "":
			fmt.Fprintf(w, "Remote: could not check (%s)\n", r.Error)
		case res.Stale:
			fmt.Fprintf(w, "Remote bundle: %s (%s) — newer than this repo. Run `%s sync`.\n", orDash(r.BundleVersion), short(r.BundleHash), a.Prog)
		default:
			fmt.Fprintln(w, "Remote bundle: up to date")
		}
	}
	modified := 0
	for _, f := range res.Files {
		if f.State != "ok" {
			modified++
			detail := ""
			if f.Detail != "" {
				detail = " — " + f.Detail
			}
			fmt.Fprintf(w, "  %-9s %s%s\n", f.State, f.Path, detail)
		}
	}
	if res.Synced {
		if modified == 0 {
			fmt.Fprintf(w, "All %d managed file(s) match the last sync.\n", len(res.Files))
		} else {
			fmt.Fprintf(w, "%d of %d managed file(s) drifted. `%s sync` restores the team version; `%s diff` shows it.\n", modified, len(res.Files), a.Prog, a.Prog)
		}
	}
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return orDash(h)
}

func (a *App) cmdDiff(ctx context.Context, args []string) error {
	fs := a.flags("diff", "[--tools …] [flags]")
	toolsFlag := fs.String("tools", "", "comma-separated tool targets")
	exitCodeFlag := fs.Bool("exit-code", false, "exit 1 when sync would change anything")
	projectFlag := fs.String("project", "", "team space id")
	apiFlag := fs.String("api-url", "", "API base URL")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	s, err := a.projectSession(*apiFlag, *projectFlag)
	if err != nil {
		return err
	}
	tools, err := toolsFor(*toolsFlag, s)
	if err != nil {
		return err
	}
	prev, _, err := syncer.LoadManifest(s.repo.Root)
	if err != nil {
		return err
	}
	body, _, err := s.client.FetchBundle(ctx, s.projectID, "")
	if err != nil {
		return err
	}
	bundle, err := syncer.ParseBundle(body)
	if err != nil {
		return err
	}
	plan, err := syncer.BuildPlan(s.repo.Root, bundle, prev, syncer.Options{Tools: tools})
	if err != nil {
		return err
	}
	for _, warn := range plan.Warnings {
		fmt.Fprintf(a.Stderr, "warning: %s\n", warn)
	}
	pending := plan.Pending()
	for _, c := range pending {
		fmt.Fprint(a.Stdout, syncer.ChangeDiff(c))
	}
	if *exitCodeFlag && len(pending) > 0 {
		return silentExit(ExitOutOfSync)
	}
	return nil
}
