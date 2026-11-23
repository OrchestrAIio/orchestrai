package syncer

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Action is what sync does to one path.
type Action string

// Actions.
const (
	ActCreate    Action = "create"
	ActUpdate    Action = "update"
	ActDelete    Action = "delete"
	ActUnchanged Action = "unchanged"
)

// Change is the planned effect on one file.
type Change struct {
	Path         string `json:"path"`
	Tool         string `json:"tool,omitempty"`
	Strategy     string `json:"strategy"`
	Action       Action `json:"action"`
	Dropped      bool   `json:"dropped,omitempty"`     // removed because the bundle no longer lists it
	LocalEdits   bool   `json:"local_edits,omitempty"` // owned content had been edited locally
	RealPath     string `json:"-"`
	Before       []byte `json:"-"`
	BeforeExists bool   `json:"-"`
	After        []byte `json:"-"`
}

// Kept is a dropped path left in place because it was edited locally.
type Kept struct {
	Path string `json:"path"`
	Tool string `json:"tool,omitempty"`
}

// Plan is the full effect of applying a bundle.
type Plan struct {
	Changes  []Change
	Kept     []Kept
	Warnings []string
	Manifest *Manifest
}

// Options control a plan.
type Options struct {
	// Tools limits which tool targets are written; nil means all tools.
	// Shared files (AGENTS.md and other tool-agnostic targets) are always
	// included.
	Tools []string
}

func (o Options) selected(tool string) bool {
	if len(o.Tools) == 0 || SharedTool(tool) {
		return true
	}
	t := strings.ToLower(tool)
	for _, s := range o.Tools {
		if strings.ToLower(s) == t {
			return true
		}
	}
	return false
}

// Pending returns the changes that modify the repository.
func (p *Plan) Pending() []Change {
	var out []Change
	for _, c := range p.Changes {
		if c.Action != ActUnchanged {
			out = append(out, c)
		}
	}
	return out
}

// OutOfSync reports whether applying the plan would change files or the
// manifest's ownership record.
func (p *Plan) OutOfSync(prev *Manifest, prevExists bool) bool {
	if len(p.Pending()) > 0 {
		return true
	}
	if !prevExists {
		return len(p.Manifest.Files) > 0
	}
	return !prev.SameFiles(p.Manifest)
}

func readIfExists(p string) ([]byte, bool, error) {
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// BuildPlan computes what applying bundle b to root would do. Any unsafe
// path or unparseable local file aborts the whole plan so nothing is
// written half-way.
func BuildPlan(root string, b *Bundle, prev *Manifest, opts Options) (*Plan, error) {
	if prev == nil {
		prev = NewManifest()
	}
	realRoot, err := RealRoot(root)
	if err != nil {
		return nil, err
	}
	plan := &Plan{Manifest: NewManifest()}
	plan.Manifest.BundleVersion = string(b.Version)
	plan.Manifest.BundleHash = b.Hash
	plan.Manifest.Tools = normalizeTools(opts.Tools)

	files := append([]File(nil), b.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	inBundle := map[string]bool{}
	realOwner := map[string]string{}

	carry := func(path string) {
		if h, ok := prev.Files[path]; ok {
			plan.Manifest.Files[path] = h
			if e, ok := prev.Entries[path]; ok {
				plan.Manifest.Entries[path] = e
			}
		}
	}

	for _, f := range files {
		inBundle[f.Path] = true
		if !opts.selected(f.Tool) {
			carry(f.Path)
			continue
		}
		strat, err := strategyFor(f.Strategy, f.Path)
		if err != nil {
			return nil, err
		}
		rp, err := Resolve(realRoot, f.Path)
		if err != nil {
			return nil, err
		}
		if other, dup := realOwner[rp]; dup {
			return nil, &PathError{Path: f.Path, Reason: "resolves to the same file as " + other + " (symlink); only one of them can be managed"}
		}
		realOwner[rp] = f.Path
		cur, exists, err := readIfExists(rp)
		if err != nil {
			return nil, err
		}
		after, warns, err := strat.Apply(cur, exists, f.Content)
		if err != nil {
			return nil, err
		}
		plan.Warnings = append(plan.Warnings, warns...)
		hash, _, err := strat.Owned(after)
		if err != nil {
			return nil, err
		}
		ch := Change{
			Path: f.Path, Tool: f.Tool, Strategy: f.Strategy, RealPath: rp,
			Before: cur, BeforeExists: exists, After: after,
		}
		switch {
		case !exists:
			ch.Action = ActCreate
		case bytes.Equal(cur, after):
			ch.Action = ActUnchanged
		default:
			ch.Action = ActUpdate
		}
		if prevHash, ok := prev.Files[f.Path]; ok && exists && ch.Action != ActUnchanged {
			if h, found, err := strat.Owned(cur); err == nil && found && h != prevHash && h != hash {
				ch.LocalEdits = true
				plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: local edits to OrchestrAI-managed content will be replaced by the team version", f.Path))
			}
		}
		plan.Changes = append(plan.Changes, ch)
		plan.Manifest.Files[f.Path] = hash
		plan.Manifest.Entries[f.Path] = ManifestEntry{Strategy: f.Strategy, Tool: f.Tool}
	}

	var dropped []string
	for p := range prev.Files {
		if !inBundle[p] {
			dropped = append(dropped, p)
		}
	}
	sort.Strings(dropped)
	for _, p := range dropped {
		entry := prev.Entries[p]
		if !opts.selected(entry.Tool) {
			carry(p)
			continue
		}
		rp, err := Resolve(realRoot, p)
		if err != nil {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: no longer in the team bundle; not removed (%v)", p, err))
			continue
		}
		if _, dup := realOwner[rp]; dup {
			continue
		}
		cur, exists, err := readIfExists(rp)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		stratName := entry.Strategy
		if stratName == "" {
			stratName = inferStrategy(p, cur)
		}
		strat, err := strategyFor(stratName, p)
		if err != nil {
			return nil, err
		}
		h, found, err := strat.Owned(cur)
		if err != nil {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: no longer in the team bundle; left in place (%v)", p, err))
			plan.Kept = append(plan.Kept, Kept{Path: p, Tool: entry.Tool})
			continue
		}
		if !found {
			continue
		}
		if h != prev.Files[p] {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: no longer in the team bundle but edited locally since the last sync; left in place", p))
			plan.Kept = append(plan.Kept, Kept{Path: p, Tool: entry.Tool})
			continue
		}
		out, del, err := strat.Remove(cur)
		if err != nil {
			return nil, err
		}
		ch := Change{Path: p, Tool: entry.Tool, Strategy: stratName, RealPath: rp, Before: cur, BeforeExists: true, After: out, Dropped: true}
		switch {
		case del:
			ch.Action, ch.After = ActDelete, nil
		case bytes.Equal(out, cur):
			ch.Action = ActUnchanged
		default:
			ch.Action = ActUpdate
		}
		plan.Changes = append(plan.Changes, ch)
	}
	sort.SliceStable(plan.Changes, func(i, j int) bool { return plan.Changes[i].Path < plan.Changes[j].Path })
	return plan, nil
}

// Apply writes the plan to disk and saves the new manifest.
func Apply(root string, plan *Plan) error {
	realRoot, err := RealRoot(root)
	if err != nil {
		return err
	}
	for _, c := range plan.Pending() {
		switch c.Action {
		case ActDelete:
			if err := os.Remove(c.RealPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove %s: %w", c.Path, err)
			}
			removeEmptyParents(realRoot, filepath.Dir(c.RealPath))
		case ActCreate, ActUpdate:
			if err := os.MkdirAll(filepath.Dir(c.RealPath), 0o755); err != nil {
				return fmt.Errorf("create directory for %s: %w", c.Path, err)
			}
			mode := fs.FileMode(0o644)
			if fi, err := os.Stat(c.RealPath); err == nil {
				mode = fi.Mode().Perm()
			}
			if err := writeAtomic(c.RealPath, c.After, mode); err != nil {
				return fmt.Errorf("write %s: %w", c.Path, err)
			}
		}
	}
	return plan.Manifest.Save(root)
}

func writeAtomic(p string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), ".orchestrai-tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	return os.Rename(name, p)
}

func removeEmptyParents(realRoot, dir string) {
	for within(realRoot, dir) && dir != realRoot {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// FileState is the local drift state of one managed path.
type FileState struct {
	Path     string `json:"path"`
	Tool     string `json:"tool,omitempty"`
	Strategy string `json:"strategy"`
	State    string `json:"state"` // ok | modified | missing | invalid
	Detail   string `json:"detail,omitempty"`
}

// LocalStatus compares every managed path against the manifest.
func LocalStatus(root string, m *Manifest) ([]FileState, error) {
	realRoot, err := RealRoot(root)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(m.Files))
	for p := range m.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]FileState, 0, len(paths))
	for _, p := range paths {
		e := m.Entries[p]
		st := FileState{Path: p, Tool: e.Tool, Strategy: e.Strategy}
		rp, err := Resolve(realRoot, p)
		if err != nil {
			st.State, st.Detail = "invalid", err.Error()
			out = append(out, st)
			continue
		}
		cur, exists, err := readIfExists(rp)
		if err != nil {
			return nil, err
		}
		if !exists {
			st.State = "missing"
			out = append(out, st)
			continue
		}
		if st.Strategy == "" {
			st.Strategy = inferStrategy(p, cur)
		}
		strat, err := strategyFor(st.Strategy, p)
		if err != nil {
			return nil, err
		}
		h, found, err := strat.Owned(cur)
		switch {
		case err != nil:
			st.State, st.Detail = "invalid", err.Error()
		case !found && m.Files[p] != "":
			st.State = "missing"
		case found && h != m.Files[p]:
			st.State = "modified"
		default:
			st.State = "ok"
		}
		out = append(out, st)
	}
	return out, nil
}

// Clean reports whether every state is ok.
func Clean(states []FileState) bool {
	for _, s := range states {
		if s.State != "ok" {
			return false
		}
	}
	return true
}
