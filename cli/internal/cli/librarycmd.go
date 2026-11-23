package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"unicode/utf8"

	"github.com/OrchestrAIio/orchestrai/cli/internal/api"
	"github.com/OrchestrAIio/orchestrai/cli/internal/library"
	"github.com/OrchestrAIio/orchestrai/cli/internal/project"
	"github.com/OrchestrAIio/orchestrai/cli/internal/syncer"
)

// resolveItem turns publisher/slug[@v] or id[@v] into a library item.
func (a *App) resolveItem(ctx context.Context, c *api.Client, projectID, ref string) (*api.LibraryItem, string, error) {
	pub, slug, id, version, err := api.ParseRef(ref)
	if err != nil {
		return nil, "", usagef("%v", err)
	}
	if id != "" {
		it, err := c.GetLibraryItem(ctx, id, version)
		if err != nil {
			return nil, "", err
		}
		if it.ID == "" {
			it.ID = id
		}
		return it, version, nil
	}
	it, err := c.GetPublicLibraryItem(ctx, pub, slug, version)
	if err == nil {
		return it, version, nil
	}
	if !api.IsStatus(err, http.StatusNotFound) || projectID == "" {
		return nil, "", err
	}
	// Not public: look in the team scope.
	items, lerr := c.ListLibraryItems(ctx, "team:"+projectID, "", slug)
	if lerr != nil {
		return nil, "", err
	}
	for i := range items {
		if items[i].Slug == slug && (items[i].Publisher == "" || items[i].Publisher == pub) {
			return &items[i], version, nil
		}
	}
	return nil, "", fmt.Errorf("library item %s/%s not found", pub, slug)
}

func (a *App) cmdAdd(ctx context.Context, args []string) error {
	fs := a.flags("add", "<publisher/slug|item-id>[@version] [flags]")
	localFlag := fs.Bool("local", false, "install directly into this workspace without signing in or binding a team space")
	noSync := fs.Bool("no-sync", false, "install without running sync afterwards")
	asJSON := fs.Bool("json", false, "print JSON")
	projectFlag := fs.String("project", "", "team space id")
	createFlag := fs.String("create-space", "", "when the repository is not bound: create a team space with this name and bind it")
	yes := fs.Bool("yes", false, "never prompt: when the repository is not bound, bind the only team space (or create one named after the repository)")
	apiFlag := fs.String("api-url", "", "API base URL")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("add needs exactly one library reference, e.g. `%s add acme/code-review@1.2.0`", a.Prog)
	}
	if *projectFlag != "" && *createFlag != "" {
		return usagef("pass either --project or --create-space, not both")
	}
	if *localFlag && (*projectFlag != "" || *createFlag != "") {
		return usagef("pass either --local or --project/--create-space, not both")
	}

	ref := fs.Arg(0)
	pub, slug, _, version, err := api.ParseRef(ref)
	if err != nil {
		return usagef("%v", err)
	}
	isPublicRef := pub != "" && slug != ""
	if *localFlag && !isPublicRef {
		return usagef("--local requires a public gallery reference (<publisher>/<slug>[@version])")
	}

	rc, err := a.repo()
	if err != nil {
		return err
	}
	u, err := a.apiURL(*apiFlag, rc.Cfg)
	if err != nil {
		return err
	}

	// Use anonymous local install when:
	//  (a) the user passed --local, OR
	//  (b) the ref is publisher/slug[@version], the user didn't explicitly ask
	//      for team space binding (--project / --create-space / --yes), and the
	//      user is either not logged in or this repo is not bound to a team space.
	useLocal := *localFlag
	if !useLocal && isPublicRef && *projectFlag == "" && *createFlag == "" && !*yes {
		bound := (rc.Bound && rc.Cfg != nil && rc.Cfg.ProjectID != "") || os.Getenv(EnvProjectID) != ""
		_, _, authErr := a.authedClient(u)
		loggedIn := authErr == nil
		if !loggedIn || !bound {
			useLocal = true
		}
	}
	if useLocal {
		return a.addLocal(ctx, rc, u, pub, slug, version, *asJSON)
	}

	s, err := a.onboardSession(ctx, *apiFlag, *projectFlag, onboardOpts{createSpace: *createFlag, yes: *yes})
	if err != nil {
		return err
	}
	item, version, err := a.resolveItem(ctx, s.client, s.projectID, ref)
	if err != nil {
		return err
	}
	inst, err := s.client.CreateInstall(ctx, s.projectID, item.ID, version)
	if err != nil {
		return err
	}
	v := inst.Version
	if v == "" {
		v = orDash(version)
	}
	out := map[string]any{"install": inst, "item": item}
	if !*asJSON {
		fmt.Fprintf(a.Stdout, "Installed %s@%s (%s) into the team space.\n", item.Ref(), v, item.Kind)
	}
	if !*noSync {
		res, err := a.runSync(ctx, s, syncOpts{mode: modeApply, tools: mustTools(s)})
		if err != nil {
			return fmt.Errorf("installed, but sync failed: %w", err)
		}
		out["sync"] = res
		if !*asJSON {
			a.printSyncSummary(a.Stdout, res)
		}
	}
	if *asJSON {
		return a.printJSON(out)
	}
	return nil
}

// addLocal fetches a public gallery item anonymously (no auth header) and
// writes its tool files directly into the workspace + updates .orchestrai/manifest.json.
func (a *App) addLocal(ctx context.Context, rc *repoContext, apiURL, pub, slug, version string, asJSON bool) error {
	anon := a.newClient(apiURL, "")
	item, err := anon.GetPublicLibraryItem(ctx, pub, slug, version)
	if err != nil {
		return err
	}
	if item.Publisher == "" {
		item.Publisher = pub
	}
	if item.Slug == "" {
		item.Slug = slug
	}
	files, err := localFilesForItem(rc.Root, item)
	if err != nil {
		return err
	}
	prev, prevExists, err := syncer.LoadManifest(rc.Root)
	if err != nil {
		return err
	}
	bundle := &syncer.Bundle{
		Version: syncer.FlexString(prev.BundleVersion),
		Hash:    prev.BundleHash,
		Files:   files,
	}
	// Build against an empty prev manifest so previously installed local/team
	// files are never treated as dropped and deleted, then merge prev back in.
	plan, err := syncer.BuildPlan(rc.Root, bundle, syncer.NewManifest(), syncer.Options{})
	if err != nil {
		return err
	}
	if prevExists {
		for p, h := range prev.Files {
			if _, replaced := plan.Manifest.Files[p]; !replaced {
				plan.Manifest.Files[p] = h
				if e, ok := prev.Entries[p]; ok {
					plan.Manifest.Entries[p] = e
				}
			}
		}
		plan.Manifest.ETag = prev.ETag
		plan.Manifest.Tools = prev.Tools
	}
	if err := syncer.Apply(rc.Root, plan); err != nil {
		return err
	}
	v := version
	if v == "" {
		v = item.DisplayVersion()
	}
	v = orDash(v)
	ref := item.Ref()
	if ref == "" {
		ref = pub + "/" + slug
	}
	changes := plan.Pending()
	if asJSON {
		return a.printJSON(map[string]any{
			"local":   true,
			"ref":     ref,
			"version": v,
			"item":    item,
			"changes": changes,
		})
	}
	for _, c := range changes {
		tool := ""
		if c.Tool != "" {
			tool = " (" + c.Tool + ")"
		}
		fmt.Fprintf(a.Stdout, "  %-16s %s%s\n", string(c.Action), c.Path, tool)
	}
	fmt.Fprintf(a.Stdout, "Installed %s@%s locally (no sign-in required).\n", ref, v)
	fmt.Fprintf(a.Stdout, "Sync rules, skills & MCP servers across your whole team & CI (free for up to 5): %s login\n", a.Prog)
	return nil
}

type localItemBody struct {
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	BodyMD       string            `json:"body_md"`
	Instructions string            `json:"instructions"`
	PathGlob     string            `json:"path_glob"`
	URL          string            `json:"url"`
	Purpose      string            `json:"purpose"`
	ArgumentHint string            `json:"argument_hint"`
	Flavour      string            `json:"flavour"`
	Transport    string            `json:"transport"`
	Command      string            `json:"command"`
	Args         []string          `json:"args"`
	Env          map[string]string `json:"env"`
	Headers      map[string]string `json:"headers"`
	AgentCommand *struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Prompt      string `json:"prompt"`
	} `json:"agent_command"`
	Files []api.LibraryFile `json:"files"`
}

func decodeLocalFileText(f api.LibraryFile) (string, bool) {
	if f.Encoding == "base64" {
		raw, err := base64.StdEncoding.DecodeString(f.Content)
		if err != nil || !utf8.Valid(raw) || bytes.ContainsRune(raw, 0) {
			return "", false
		}
		return string(raw), true
	}
	return f.Content, true
}

func localEnsureNL(s string) string {
	s = strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if s == "" {
		return ""
	}
	return s + "\n"
}

func localYAMLString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

var localPlaceholderRe = regexp.MustCompile(`\$\{(?:env:)?([A-Za-z_][A-Za-z0-9_]*)\}`)

func localRenderPlaceholders(s, style string) string {
	return localPlaceholderRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := localPlaceholderRe.FindStringSubmatch(m)
		if len(sub) < 2 {
			return m
		}
		if style == "env" {
			return "${env:" + sub[1] + "}"
		}
		return "${" + sub[1] + "}"
	})
}

// mergeManagedSection preserves existing text inside an orchestrai managed block
// on disk when adding a new local item section (idempotent if already present).
func mergeManagedSection(root, relPath, begin, end, addition string) string {
	addition = localEnsureNL(strings.TrimSpace(addition))
	if addition == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relPath)))
	if err != nil {
		return addition
	}
	text := string(syncer.NormalizeLF(data))
	bi := strings.Index(text, begin+"\n")
	if bi < 0 {
		return addition
	}
	rest := text[bi+len(begin)+1:]
	ei := strings.Index(rest, end)
	if ei < 0 {
		return addition
	}
	existing := strings.TrimSpace(rest[:ei])
	if existing == "" {
		return addition
	}
	if strings.Contains(existing, strings.TrimSpace(addition)) {
		return localEnsureNL(existing)
	}
	return localEnsureNL(existing + "\n\n" + strings.TrimSpace(addition))
}

func localFilesForItem(root string, item *api.LibraryItem) ([]syncer.File, error) {
	var body localItemBody
	if len(item.Body) > 0 {
		_ = json.Unmarshal(item.Body, &body)
	}
	slug := library.Slugify(item.Slug)
	if slug == "" {
		slug = library.Slugify(item.Name)
	}
	if slug == "" {
		return nil, &syncer.PathError{Path: item.Slug, Reason: "item has no safe slug"}
	}
	title := item.Name
	if title == "" {
		title = slug
	}
	desc := body.Description
	if desc == "" {
		desc = item.Description
	}
	sourceFiles := item.Files
	if len(sourceFiles) == 0 && len(body.Files) > 0 {
		sourceFiles = body.Files
	}

	firstText := func() string {
		for _, f := range sourceFiles {
			if txt, ok := decodeLocalFileText(f); ok && strings.TrimSpace(txt) != "" {
				return txt
			}
		}
		return ""
	}

	var out []syncer.File
	emit := func(rel, tool, strategy, content string) error {
		if err := syncer.ValidateRelPath(rel); err != nil {
			return err
		}
		out = append(out, syncer.File{
			Path:     rel,
			Tool:     tool,
			Strategy: strategy,
			Content:  content,
			SHA256:   syncer.SHA256Hex([]byte(content)),
		})
		return nil
	}

	switch item.Kind {
	case "skill", "":
		wrote := 0
		for _, f := range sourceFiles {
			txt, ok := decodeLocalFileText(f)
			if !ok {
				continue
			}
			rel := path.Clean(strings.TrimPrefix(f.Path, "/"))
			if err := syncer.ValidateRelPath(rel); err != nil {
				return nil, err
			}
			if err := emit(".claude/skills/"+slug+"/"+rel, "claude", syncer.StrategyFile, localEnsureNL(txt)); err != nil {
				return nil, err
			}
			if err := emit(".agents/skills/"+slug+"/"+rel, "shared", syncer.StrategyFile, localEnsureNL(txt)); err != nil {
				return nil, err
			}
			wrote++
		}
		if wrote == 0 {
			md := body.BodyMD
			if md == "" {
				md = fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s\n", localYAMLString(title), localYAMLString(desc), desc)
			}
			if err := emit(".claude/skills/"+slug+"/SKILL.md", "claude", syncer.StrategyFile, localEnsureNL(md)); err != nil {
				return nil, err
			}
			if err := emit(".agents/skills/"+slug+"/SKILL.md", "shared", syncer.StrategyFile, localEnsureNL(md)); err != nil {
				return nil, err
			}
		}

	case "rule":
		md := strings.TrimSpace(body.BodyMD)
		if md == "" {
			md = strings.TrimSpace(firstText())
		}
		if md == "" {
			md = desc
		}
		pathGlob := strings.TrimSpace(body.PathGlob)
		if pathGlob == "" && md != "" {
			fm := library.ParseFrontmatter(md)
			if g, ok := fm["globs"].(string); ok {
				pathGlob = g
			} else if g, ok := fm["applyTo"].(string); ok {
				pathGlob = g
			}
		}
		head := "### " + title
		if pathGlob != "" {
			head += " (applies to `" + pathGlob + "`)"
		}
		agentsContent := mergeManagedSection(root, "AGENTS.md", syncer.MDBegin, syncer.MDEnd, head+"\n\n"+md)
		if err := emit("AGENTS.md", "shared", syncer.StrategyManagedBlock, agentsContent); err != nil {
			return nil, err
		}
		claudeContent := mergeManagedSection(root, "CLAUDE.md", syncer.MDBegin, syncer.MDEnd, "@AGENTS.md")
		if err := emit("CLAUDE.md", "claude", syncer.StrategyManagedBlock, claudeContent); err != nil {
			return nil, err
		}
		if pathGlob != "" {
			d := desc
			if d == "" {
				d = title
			}
			mdc := fmt.Sprintf("---\ndescription: %s\nglobs: %s\nalwaysApply: false\n---\n%s", localYAMLString(d), pathGlob, localEnsureNL(md))
			if err := emit(".cursor/rules/"+slug+".mdc", "cursor", syncer.StrategyFile, mdc); err != nil {
				return nil, err
			}
			inst := fmt.Sprintf("---\napplyTo: %s\ndescription: %s\n---\n%s", localYAMLString(pathGlob), localYAMLString(d), localEnsureNL(md))
			if err := emit(".github/instructions/"+slug+".instructions.md", "vscode", syncer.StrategyFile, inst); err != nil {
				return nil, err
			}
		}

	case "workflow", "prompt":
		md := strings.TrimSpace(body.BodyMD)
		if md == "" && body.AgentCommand != nil {
			md = strings.TrimSpace(body.AgentCommand.Prompt)
			if desc == "" {
				desc = body.AgentCommand.Description
			}
		}
		if md == "" {
			md = strings.TrimSpace(firstText())
		}
		if md == "" {
			md = desc
		}
		var fm strings.Builder
		fm.WriteString("---\n")
		if desc != "" {
			fm.WriteString("description: " + localYAMLString(desc) + "\n")
		}
		if body.ArgumentHint != "" {
			fm.WriteString("argument-hint: " + localYAMLString(body.ArgumentHint) + "\n")
		}
		fm.WriteString("---\n")
		text := localEnsureNL(md)
		if err := emit(".claude/commands/"+slug+".md", "claude", syncer.StrategyFile, fm.String()+text); err != nil {
			return nil, err
		}
		if err := emit(".cursor/commands/"+slug+".md", "cursor", syncer.StrategyFile, text); err != nil {
			return nil, err
		}
		var pf strings.Builder
		pf.WriteString("---\nname: " + localYAMLString(slug) + "\n")
		if desc != "" {
			pf.WriteString("description: " + localYAMLString(desc) + "\n")
		}
		if body.ArgumentHint != "" {
			pf.WriteString("argument-hint: " + localYAMLString(body.ArgumentHint) + "\n")
		}
		pf.WriteString("agent: agent\n---\n")
		if err := emit(".github/prompts/"+slug+".prompt.md", "vscode", syncer.StrategyFile, pf.String()+text); err != nil {
			return nil, err
		}

	case "subagent":
		md := strings.TrimSpace(body.BodyMD)
		if md == "" {
			md = strings.TrimSpace(body.Instructions)
		}
		if md == "" {
			md = strings.TrimSpace(firstText())
		}
		if md == "" {
			md = desc
		}
		claudeDoc := md
		if !strings.HasPrefix(claudeDoc, "---\n") {
			claudeDoc = fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n%s", localYAMLString(slug), localYAMLString(desc), localEnsureNL(md))
		}
		if err := emit(".claude/agents/"+slug+".md", "claude", syncer.StrategyFile, localEnsureNL(claudeDoc)); err != nil {
			return nil, err
		}
		codexDoc := fmt.Sprintf("name = %s\ndescription = %s\ninstructions = %s\n",
			localYAMLString(slug), localYAMLString(desc), localYAMLString(md))
		if err := emit(".codex/agents/"+slug+".toml", "codex", syncer.StrategyFile, codexDoc); err != nil {
			return nil, err
		}

	case "mcp_server":
		if body.Command == "" && body.URL == "" {
			if raw := firstText(); raw != "" {
				_ = json.Unmarshal([]byte(raw), &body)
			}
		}
		buildEntry := func(tool string) map[string]any {
			style := "plain"
			if tool == "cursor" || tool == "vscode" {
				style = "env"
			}
			e := map[string]any{"x-orchestrai": true}
			renderMap := func(m map[string]string) map[string]string {
				res := make(map[string]string, len(m))
				for k, v := range m {
					res[k] = localRenderPlaceholders(v, style)
				}
				return res
			}
			if body.Transport == "http" || (body.URL != "" && body.Command == "") {
				e["url"] = body.URL
				if tool != "cursor" {
					e["type"] = "http"
				}
				if len(body.Headers) > 0 {
					e["headers"] = renderMap(body.Headers)
				}
				return e
			}
			e["type"] = "stdio"
			e["command"] = body.Command
			args := make([]string, len(body.Args))
			for i, arg := range body.Args {
				args[i] = localRenderPlaceholders(arg, style)
			}
			e["args"] = args
			if len(body.Env) > 0 {
				e["env"] = renderMap(body.Env)
			}
			return e
		}
		marshalContainer := func(container string, entry map[string]any) string {
			raw, _ := json.MarshalIndent(map[string]any{container: map[string]any{slug: entry}}, "", "  ")
			return string(raw) + "\n"
		}
		if err := emit(".mcp.json", "claude", syncer.StrategyJSONMerge, marshalContainer("mcpServers", buildEntry("claude"))); err != nil {
			return nil, err
		}
		if err := emit(".cursor/mcp.json", "cursor", syncer.StrategyJSONMerge, marshalContainer("mcpServers", buildEntry("cursor"))); err != nil {
			return nil, err
		}
		if err := emit(".vscode/mcp.json", "vscode", syncer.StrategyJSONMerge, marshalContainer("servers", buildEntry("vscode"))); err != nil {
			return nil, err
		}
		var toml strings.Builder
		toml.WriteString("[mcp_servers." + slug + "]\n")
		if body.Transport == "http" || (body.URL != "" && body.Command == "") {
			toml.WriteString("url = " + localYAMLString(body.URL) + "\n")
		} else {
			toml.WriteString("command = " + localYAMLString(body.Command) + "\n")
			if len(body.Args) > 0 {
				parts := make([]string, len(body.Args))
				for i, arg := range body.Args {
					parts[i] = localYAMLString(arg)
				}
				toml.WriteString("args = [" + strings.Join(parts, ", ") + "]\n")
			}
		}
		codexMerged := mergeManagedSection(root, ".codex/config.toml", syncer.TOMLBegin, syncer.TOMLEnd, toml.String())
		if err := emit(".codex/config.toml", "codex", syncer.StrategyManagedBlock, codexMerged); err != nil {
			return nil, err
		}

	case "doc":
		docURL := body.URL
		if docURL == "" {
			docURL = item.URL
		}
		purpose := body.Purpose
		if purpose == "" {
			purpose = desc
		}
		line := fmt.Sprintf("- [%s](%s)", title, docURL)
		if purpose != "" {
			line += " — " + purpose
		}
		agentsContent := mergeManagedSection(root, "AGENTS.md", syncer.MDBegin, syncer.MDEnd, "## Reference docs\n\n"+line)
		if err := emit("AGENTS.md", "shared", syncer.StrategyManagedBlock, agentsContent); err != nil {
			return nil, err
		}
		claudeContent := mergeManagedSection(root, "CLAUDE.md", syncer.MDBegin, syncer.MDEnd, "@AGENTS.md")
		if err := emit("CLAUDE.md", "claude", syncer.StrategyManagedBlock, claudeContent); err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("unsupported library item kind %q", item.Kind)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func mustTools(s *session) []string {
	t, _ := toolsFor("", s)
	return t
}

func (a *App) cmdRemove(ctx context.Context, args []string) error {
	fs := a.flags("remove", "<publisher/slug|item-id|install-id> [flags]")
	noSync := fs.Bool("no-sync", false, "uninstall without running sync afterwards")
	projectFlag := fs.String("project", "", "team space id")
	apiFlag := fs.String("api-url", "", "API base URL")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("remove needs exactly one reference")
	}
	s, err := a.projectSession(*apiFlag, *projectFlag)
	if err != nil {
		return err
	}
	ref := fs.Arg(0)
	if at := strings.LastIndex(ref, "@"); at > 0 {
		ref = ref[:at]
	}
	installs, err := s.client.ListInstalls(ctx, s.projectID)
	if err != nil {
		return err
	}
	var match []api.Install
	for _, in := range installs {
		if in.ID == ref || in.ItemID == ref || in.Ref() == ref {
			match = append(match, in)
		}
	}
	switch len(match) {
	case 0:
		return fmt.Errorf("%s is not installed in this team space", ref)
	case 1:
	default:
		return usagef("%s matches %d installs; pass the install id", ref, len(match))
	}
	if err := s.client.DeleteInstall(ctx, s.projectID, match[0].ID); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "Removed %s from the team space.\n", match[0].Ref())
	if *noSync {
		return nil
	}
	res, err := a.runSync(ctx, s, syncOpts{mode: modeApply, tools: mustTools(s)})
	if err != nil {
		return fmt.Errorf("removed, but sync failed: %w", err)
	}
	a.printSyncSummary(a.Stdout, res)
	return nil
}

func (a *App) cmdLibrary(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(a.Stderr, "Usage: %s library <list|search|show|publish> [flags]\n", a.Prog)
		if len(args) == 0 {
			return usagef("missing library subcommand")
		}
		return nil
	}
	switch args[0] {
	case "list", "ls":
		return a.libraryList(ctx, args[1:], false)
	case "search":
		return a.libraryList(ctx, args[1:], true)
	case "show":
		return a.libraryShow(ctx, args[1:])
	case "publish":
		return a.libraryPublish(ctx, args[1:])
	}
	return usagef("unknown library subcommand %q (list, search, show, publish)", args[0])
}

// scopeParam maps --scope to the API's scope query value.
func scopeParam(scope, projectID, orgID string) (string, error) {
	switch scope {
	case "team":
		if projectID == "" {
			return "", project.ErrNotInitialized
		}
		return "team:" + projectID, nil
	case "org", "workplace":
		if orgID == "" {
			return "", usagef("--scope org needs --org <id> (or org_id in .orchestrai/config.json)")
		}
		return "org:" + orgID, nil
	case "community", "public":
		return "community", nil
	}
	return "", usagef("invalid --scope %q (team, org, community)", scope)
}

// librarySession resolves what library commands need; the project is
// optional (community scope works without a binding).
func (a *App) librarySession(apiFlag, projectFlag, orgFlag string) (*session, string, error) {
	s, err := a.projectSession(apiFlag, projectFlag)
	if errors.Is(err, project.ErrNotInitialized) {
		rc, rerr := a.repo()
		if rerr != nil {
			return nil, "", rerr
		}
		u, uerr := a.apiURL(apiFlag, rc.Cfg)
		if uerr != nil {
			return nil, "", uerr
		}
		c, _, cerr := a.authedClient(u)
		if cerr != nil {
			return nil, "", cerr
		}
		s = &session{repo: rc, apiURL: u, appURL: a.appURL(rc.Cfg), client: c}
	} else if err != nil {
		return nil, "", err
	}
	org := orgFlag
	if org == "" && s.repo.Cfg != nil {
		org = s.repo.Cfg.OrgID
	}
	return s, org, nil
}

func (a *App) libraryList(ctx context.Context, args []string, search bool) error {
	name := "library list"
	defScope := "team"
	if search {
		name, defScope = "library search", "community"
	}
	fs := a.flags(name, "[query] [--scope team|org|community] [--kind …] [flags]")
	scope := fs.String("scope", defScope, "team, org or community")
	kind := fs.String("kind", "", "filter by kind: "+strings.Join(library.Kinds, ", "))
	org := fs.String("org", "", "organization id for --scope org")
	asJSON := fs.Bool("json", false, "print JSON")
	projectFlag := fs.String("project", "", "team space id")
	apiFlag := fs.String("api-url", "", "API base URL")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *kind != "" && !library.ValidKind(*kind) {
		return usagef("invalid --kind %q", *kind)
	}
	q := strings.Join(fs.Args(), " ")
	if search && q == "" {
		return usagef("library search needs a query")
	}
	s, orgID, err := a.librarySession(*apiFlag, *projectFlag, *org)
	if err != nil {
		return err
	}
	sp, err := scopeParam(*scope, s.projectID, orgID)
	if err != nil {
		return err
	}
	items, err := s.client.ListLibraryItems(ctx, sp, *kind, q)
	if err != nil {
		return err
	}
	if *asJSON {
		raws := make([]json.RawMessage, 0, len(items))
		for _, it := range items {
			raws = append(raws, it.Raw)
		}
		return a.printJSON(map[string]any{"scope": sp, "items": raws})
	}
	if len(items) == 0 {
		fmt.Fprintln(a.Stdout, "No items.")
		return nil
	}
	tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "REF\tKIND\tVERSION\tNAME\tDESCRIPTION")
	for _, it := range items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", it.Ref(), it.Kind, orDash(it.DisplayVersion()), it.Name, truncate(it.Description, 60))
	}
	return tw.Flush()
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func (a *App) libraryShow(ctx context.Context, args []string) error {
	fs := a.flags("library show", "<publisher/slug|item-id>[@version] [flags]")
	content := fs.Bool("content", false, "print file contents")
	asJSON := fs.Bool("json", false, "print JSON")
	projectFlag := fs.String("project", "", "team space id")
	apiFlag := fs.String("api-url", "", "API base URL")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("library show needs one reference")
	}
	s, _, err := a.librarySession(*apiFlag, *projectFlag, "")
	if err != nil {
		return err
	}
	it, _, err := a.resolveItem(ctx, s.client, s.projectID, fs.Arg(0))
	if err != nil {
		return err
	}
	if *asJSON {
		return a.printJSON(it.Raw)
	}
	w := a.Stdout
	fmt.Fprintf(w, "%s  (%s)\n", it.Name, it.Ref())
	fmt.Fprintf(w, "  Kind:    %s\n  Version: %s\n  Scope:   %s\n", it.Kind, orDash(it.DisplayVersion()), orDash(it.Scope))
	if it.Status != "" {
		fmt.Fprintf(w, "  Status:  %s\n", it.Status)
	}
	if len(it.ToolCompat) > 0 {
		fmt.Fprintf(w, "  Tools:   %s\n", strings.Join(it.ToolCompat, ", "))
	}
	if it.InstallCount > 0 {
		fmt.Fprintf(w, "  Installs: %d\n", it.InstallCount)
	}
	if it.Description != "" {
		fmt.Fprintf(w, "\n%s\n", it.Description)
	}
	if len(it.Files) > 0 {
		fmt.Fprintln(w, "\nFiles:")
		for _, f := range it.Files {
			fmt.Fprintf(w, "  %s\n", f.Path)
		}
	}
	if *content {
		for _, f := range it.Files {
			text := f.Content
			if f.Encoding == "base64" {
				raw, err := base64.StdEncoding.DecodeString(f.Content)
				if err != nil {
					return fmt.Errorf("decode %s: %w", f.Path, err)
				}
				text = fmt.Sprintf("(binary, %d bytes)\n", len(raw))
			}
			fmt.Fprintf(w, "\n===== %s =====\n%s", f.Path, text)
			if !strings.HasSuffix(text, "\n") {
				fmt.Fprintln(w)
			}
		}
	}
	fmt.Fprintf(w, "\nInstall: %s add %s\n", a.Prog, it.Ref())
	return nil
}

func (a *App) libraryPublish(ctx context.Context, args []string) error {
	fs := a.flags("library publish", "<path|url> [--scope team|org|community] [--kind …] [flags]")
	scope := fs.String("scope", "team", "team, org or community")
	kind := fs.String("kind", "", "item kind (inferred when omitted): "+strings.Join(library.Kinds, ", "))
	name := fs.String("name", "", "display name (default: front matter name or file name)")
	slug := fs.String("slug", "", "URL slug (default: derived from the name)")
	desc := fs.String("description", "", "description (default: front matter description)")
	version := fs.String("version", "", "version to publish (default: assigned by the server)")
	itemID := fs.String("item", "", "publish a new version of this existing item id")
	org := fs.String("org", "", "organization id for --scope org")
	dry := fs.Bool("dry-run", false, "scan and show what would be uploaded; upload nothing")
	asJSON := fs.Bool("json", false, "print JSON")
	projectFlag := fs.String("project", "", "team space id")
	apiFlag := fs.String("api-url", "", "API base URL")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("library publish needs exactly one path (a skill directory, a command/agent .md file, an MCP server .json, or a doc URL)")
	}
	prep, err := library.Prepare(fs.Arg(0), *kind)
	if err != nil {
		return err
	}
	if *name != "" {
		prep.Name = *name
	}
	if prep.Name == "" {
		return usagef("pass --name for this item")
	}
	if *slug != "" {
		prep.Slug = library.Slugify(*slug)
	} else if prep.Slug == "" || *name != "" {
		prep.Slug = library.Slugify(prep.Name)
	}
	if *desc != "" {
		prep.Description = *desc
	}
	if *dry {
		total := 0
		for _, f := range prep.Files {
			total += len(f.Content)
		}
		if *asJSON {
			return a.printJSON(map[string]any{"kind": prep.Kind, "name": prep.Name, "slug": prep.Slug, "description": prep.Description, "url": prep.URL, "files": len(prep.Files), "bytes": total, "secrets": 0})
		}
		fmt.Fprintf(a.Stdout, "Would publish %s %q (slug %s) with %d file(s), %d bytes. No secrets found.\n", prep.Kind, prep.Name, prep.Slug, len(prep.Files), total)
		for _, f := range prep.Files {
			fmt.Fprintf(a.Stdout, "  %s\n", f.Path)
		}
		return nil
	}
	s, orgID, err := a.librarySession(*apiFlag, *projectFlag, *org)
	if err != nil {
		return err
	}
	sp, err := scopeParam(*scope, s.projectID, orgID)
	if err != nil {
		return err
	}
	item, created, err := s.client.PublishLibraryItem(ctx, *itemID, api.PublishRequest{
		Scope: sp, Kind: prep.Kind, Name: prep.Name, Slug: prep.Slug, Description: prep.Description,
		Version: *version, URL: prep.URL, Metadata: prep.Metadata, Files: prep.Files,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		return a.printJSON(map[string]any{"created": created, "item": item.Raw})
	}
	verb := "Published a new version of"
	if created {
		verb = "Published"
	}
	ref := item.Ref()
	if ref == "" {
		ref = prep.Slug
	}
	fmt.Fprintf(a.Stdout, "%s %s@%s (%s) to %s.\n", verb, ref, orDash(item.DisplayVersion()), prep.Kind, sp)
	if *scope == "community" && item.Publisher != "" && item.Slug != "" {
		fmt.Fprintf(a.Stdout, "Gallery page: %s/library/%s/%s\nInstall:      npx @orchestrai/cli add %s/%s\n", s.appURL, item.Publisher, item.Slug, item.Publisher, item.Slug)
	}
	return nil
}
