package syncer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ManifestRel is the manifest location relative to the repository root.
const ManifestRel = ".orchestrai/manifest.json"

// ManifestEntry records how a path is owned so dropped files can be cleaned
// up with the right strategy and tool filters can leave other tools alone.
type ManifestEntry struct {
	Strategy string `json:"strategy"`
	Tool     string `json:"tool,omitempty"`
}

// Manifest is .orchestrai/manifest.json. files[path] is the sha256 (hex) of
// the content the CLI owns in that file, computed as:
//
//   - file:          sha256 of the whole file with CRLF normalized to LF
//   - managed_block: sha256 of the text strictly between the marker lines,
//     LF-normalized (each line ends in "\n")
//   - json_merge:    sha256 of {"<container>": {"<name>": entry, ...}} over
//     the entries flagged "x-orchestrai": true, serialized with sorted keys,
//     no whitespace and no HTML escaping
//
// The editor extension computes the same hashes so either client can detect
// drift in a repository synced by the other.
type Manifest struct {
	BundleVersion string                   `json:"bundle_version"`
	BundleHash    string                   `json:"bundle_hash"`
	Files         map[string]string        `json:"files"`
	Entries       map[string]ManifestEntry `json:"entries,omitempty"`
	Tools         []string                 `json:"tools,omitempty"`
	ETag          string                   `json:"etag,omitempty"`
}

// NewManifest returns an empty manifest.
func NewManifest() *Manifest {
	return &Manifest{Files: map[string]string{}, Entries: map[string]ManifestEntry{}}
}

// LoadManifest reads the manifest; ok is false when it does not exist.
func LoadManifest(root string) (m *Manifest, ok bool, err error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(ManifestRel)))
	if errors.Is(err, fs.ErrNotExist) {
		return NewManifest(), false, nil
	}
	if err != nil {
		return nil, false, err
	}
	m = NewManifest()
	if err := json.Unmarshal(data, m); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w (delete it and run sync again to rebuild it)", ManifestRel, err)
	}
	if m.Files == nil {
		m.Files = map[string]string{}
	}
	if m.Entries == nil {
		m.Entries = map[string]ManifestEntry{}
	}
	return m, true, nil
}

// Marshal renders the manifest deterministically (sorted keys, 2-space indent).
func (m *Manifest) Marshal() []byte {
	data, _ := json.MarshalIndent(m, "", "  ")
	return append(data, '\n')
}

// Hash is the sha256 of the serialized manifest (sent in sync reports).
func (m *Manifest) Hash() string { return SHA256Hex(m.Marshal()) }

// Save writes the manifest atomically.
func (m *Manifest) Save(root string) error {
	p := filepath.Join(root, filepath.FromSlash(ManifestRel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return writeAtomic(p, m.Marshal(), 0o644)
}

// SameFiles reports whether two manifests own the same paths with the same hashes.
func (m *Manifest) SameFiles(o *Manifest) bool {
	if len(m.Files) != len(o.Files) {
		return false
	}
	for k, v := range m.Files {
		if ov, ok := o.Files[k]; !ok || ov != v {
			return false
		}
	}
	return true
}

// SameTools reports whether the manifest was produced with this tool selection.
func (m *Manifest) SameTools(tools []string) bool {
	a := normalizeTools(m.Tools)
	b := normalizeTools(tools)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func normalizeTools(tools []string) []string {
	if len(tools) == 0 {
		return nil
	}
	out := make([]string, 0, len(tools))
	seen := map[string]bool{}
	for _, t := range tools {
		t = strings.ToLower(strings.TrimSpace(t))
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// inferStrategy guesses how a path was owned when the manifest predates
// the entries map (or was written by another client without it).
func inferStrategy(rel string, content []byte) string {
	lower := strings.ToLower(rel)
	if strings.HasSuffix(lower, ".json") {
		if _, found, err := (jsonMerge{path: rel}).Owned(content); err == nil && found {
			return StrategyJSONMerge
		}
	}
	mb, _ := strategyFor(StrategyManagedBlock, rel)
	if _, found, err := mb.Owned(content); err == nil && found {
		return StrategyManagedBlock
	}
	return StrategyFile
}
