package library

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/OrchestrAIio/orchestrai/cli/internal/api"
)

// Kinds (spec §6).
var Kinds = []string{"skill", "rule", "workflow", "subagent", "mcp_server", "prompt", "doc"}

// ValidKind reports whether k is a library kind.
func ValidKind(k string) bool {
	for _, x := range Kinds {
		if x == k {
			return true
		}
	}
	return false
}

// Upload limits enforced before anything leaves the machine.
const (
	MaxFileBytes  = 1 << 20
	MaxTotalBytes = 5 << 20
	MaxFiles      = 200
)

// Prepared is a publishable item read from disk.
type Prepared struct {
	Kind        string
	Name        string
	Slug        string
	Description string
	URL         string
	Metadata    map[string]any
	Files       []api.LibraryFile
}

// SecretsError lists findings that block a publish.
type SecretsError struct{ Findings []Finding }

func (e *SecretsError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "refusing to publish: %d possible secret(s) found\n", len(e.Findings))
	for _, f := range e.Findings {
		b.WriteString("  " + f.String() + "\n")
	}
	b.WriteString("Remove them (use ${env:VAR} placeholders), or mark a false positive with a trailing `" + AllowPragma + "` comment.")
	return b.String()
}

var skipNames = map[string]bool{".git": true, "node_modules": true, ".DS_Store": true, "__pycache__": true, ".venv": true}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify lowercases and dashes a name.
func Slugify(s string) string {
	return strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// Prepare reads p (a skill directory, a single .md/.mdc/.json file, or a
// URL for a doc item) and infers its kind unless kind is given.
func Prepare(p, kind string) (*Prepared, error) {
	if kind != "" && !ValidKind(kind) {
		return nil, fmt.Errorf("unknown kind %q (one of %s)", kind, strings.Join(Kinds, ", "))
	}
	if strings.HasPrefix(p, "https://") || strings.HasPrefix(p, "http://") {
		if kind != "" && kind != "doc" {
			return nil, fmt.Errorf("a URL can only be published as kind doc")
		}
		return &Prepared{Kind: "doc", URL: p, Metadata: map[string]any{}}, nil
	}
	fi, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() && strings.EqualFold(filepath.Base(p), "SKILL.md") {
		p = filepath.Dir(p)
		fi, err = os.Stat(p)
		if err != nil {
			return nil, err
		}
	}
	var prep *Prepared
	if fi.IsDir() {
		prep, err = prepareDir(p, kind)
	} else {
		prep, err = prepareFile(p, kind)
	}
	if err != nil {
		return nil, err
	}
	var findings []Finding
	for _, f := range prep.Files {
		text := f.Content
		if f.Encoding == "base64" {
			raw, _ := base64.StdEncoding.DecodeString(f.Content)
			text = string(raw)
		}
		findings = append(findings, ScanSecrets(f.Path, text)...)
	}
	if prep.Kind == "mcp_server" {
		findings = append(findings, scanMCPEnv(prep.Files)...)
	}
	if len(findings) > 0 {
		return nil, &SecretsError{Findings: findings}
	}
	if prep.Slug == "" {
		prep.Slug = Slugify(prep.Name)
	}
	if prep.Slug == "" {
		return nil, fmt.Errorf("could not derive a slug from %q; pass --name", p)
	}
	return prep, nil
}

func fileEntry(rel string, data []byte) api.LibraryFile {
	sum := sha256.Sum256(data)
	f := api.LibraryFile{Path: rel, SHA256: hex.EncodeToString(sum[:])}
	if utf8.Valid(data) && !bytes.ContainsRune(data, 0) {
		f.Content = string(data)
	} else {
		f.Content = base64.StdEncoding.EncodeToString(data)
		f.Encoding = "base64"
	}
	return f
}

func prepareDir(dir, kind string) (*Prepared, error) {
	skillPath := filepath.Join(dir, "SKILL.md")
	if _, err := os.Stat(skillPath); err != nil {
		if kind == "" {
			return nil, fmt.Errorf("%s has no SKILL.md; pass a skill directory or a single file (or --kind)", dir)
		}
		if kind == "skill" {
			return nil, fmt.Errorf("a skill directory needs a SKILL.md")
		}
	}
	if kind == "" {
		kind = "skill"
	}
	var files []api.LibraryFile
	total := 0
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skipNames[d.Name()] && path != root {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; publish real files only", path)
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > MaxFileBytes {
			return fmt.Errorf("%s is larger than %d KiB", path, MaxFileBytes>>10)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		total += len(data)
		if total > MaxTotalBytes {
			return fmt.Errorf("%s is larger than %d MiB in total", dir, MaxTotalBytes>>20)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, fileEntry(filepath.ToSlash(rel), data))
		if len(files) > MaxFiles {
			return fmt.Errorf("%s has more than %d files", dir, MaxFiles)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	prep := &Prepared{Kind: kind, Files: files, Metadata: map[string]any{}}
	if data, err := os.ReadFile(skillPath); err == nil {
		fm := ParseFrontmatter(string(data))
		prep.Metadata = fm
		prep.Name, _ = fm["name"].(string)
		prep.Description, _ = fm["description"].(string)
	}
	if prep.Name == "" {
		prep.Name = filepath.Base(root)
	}
	return prep, nil
}

func prepareFile(p, kind string) (*Prepared, error) {
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if info.Size() > MaxFileBytes {
		return nil, fmt.Errorf("%s is larger than %d KiB", p, MaxFileBytes>>10)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	base := filepath.Base(p)
	prep := &Prepared{Files: []api.LibraryFile{fileEntry(base, data)}, Metadata: map[string]any{}}
	lower := strings.ToLower(base)
	if strings.HasSuffix(lower, ".json") {
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("%s is not valid JSON: %w", p, err)
		}
		if kind == "" {
			kind = "mcp_server"
		}
		if name, ok := doc["name"].(string); ok {
			prep.Name = name
		}
		if d, ok := doc["description"].(string); ok {
			prep.Description = d
		}
	} else {
		fm := ParseFrontmatter(string(data))
		prep.Metadata = fm
		prep.Name, _ = fm["name"].(string)
		prep.Description, _ = fm["description"].(string)
		if kind == "" {
			kind = inferKind(p, lower, fm)
		}
	}
	if prep.Name == "" {
		name := base
		for _, suf := range []string{".instructions.md", ".prompt.md", ".agent.md", ".md", ".mdc", ".json", ".txt"} {
			if strings.HasSuffix(strings.ToLower(name), suf) {
				name = name[:len(name)-len(suf)]
				break
			}
		}
		prep.Name = name
	}
	prep.Kind = kind
	return prep, nil
}

func inferKind(p, lowerBase string, fm map[string]any) string {
	segs := strings.Split(filepath.ToSlash(strings.ToLower(filepath.Clean(p))), "/")
	hasSeg := func(s string) bool {
		for _, x := range segs[:len(segs)-1] {
			if x == s {
				return true
			}
		}
		return false
	}
	_, hasTools := fm["tools"]
	_, hasName := fm["name"]
	switch {
	case strings.HasSuffix(lowerBase, ".mdc"), strings.HasSuffix(lowerBase, ".instructions.md"),
		lowerBase == "agents.md", lowerBase == "claude.md":
		return "rule"
	case strings.HasSuffix(lowerBase, ".prompt.md"), hasSeg("commands"), hasSeg("prompts"):
		return "workflow"
	case strings.HasSuffix(lowerBase, ".agent.md"), hasSeg("agents"):
		return "subagent"
	}
	if _, ok := fm["allowed-tools"]; ok {
		return "workflow"
	}
	if _, ok := fm["argument-hint"]; ok {
		return "workflow"
	}
	if hasTools && hasName {
		return "subagent"
	}
	for _, k := range []string{"globs", "applyTo", "alwaysApply", "paths"} {
		if _, ok := fm[k]; ok {
			return "rule"
		}
	}
	return "prompt"
}

// scanMCPEnv flags literal values in an MCP server's env/headers: secrets
// there must be ${env:VAR} placeholders.
func scanMCPEnv(files []api.LibraryFile) []Finding {
	var out []Finding
	for _, f := range files {
		var doc map[string]any
		if json.Unmarshal([]byte(f.Content), &doc) != nil {
			continue
		}
		var walk func(v any)
		walk = func(v any) {
			switch t := v.(type) {
			case map[string]any:
				for k, val := range t {
					if (k == "env" || k == "headers") && val != nil {
						if m, ok := val.(map[string]any); ok {
							for ek, ev := range m {
								s, ok := ev.(string)
								if !ok || s == "" || strings.Contains(s, "${") || isPlaceholder(s) {
									continue
								}
								if looksSecretKey(ek) {
									out = append(out, Finding{Path: f.Path, Line: 0, Rule: "literal value for " + k + "." + ek + " (use ${env:" + strings.ToUpper(Slugify(ek)) + "})", Snip: redact(s)})
								}
							}
						}
						continue
					}
					walk(val)
				}
			case []any:
				for _, e := range t {
					walk(e)
				}
			}
		}
		walk(doc)
	}
	return out
}

func looksSecretKey(k string) bool {
	l := strings.ToLower(k)
	for _, h := range []string{"key", "token", "secret", "password", "auth", "credential", "pat"} {
		if strings.Contains(l, h) {
			return true
		}
	}
	return false
}

// ParseFrontmatter reads a leading YAML front-matter block (--- … ---) into
// a flat map. Scalars become strings; inline [a, b] and "- item" lists
// become []string. Nested mappings are kept as their raw text.
func ParseFrontmatter(text string) map[string]any {
	out := map[string]any{}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return out
	}
	rest := text[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return out
	}
	lastKey := ""
	for _, line := range strings.Split(rest[:end], "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			t := strings.TrimSpace(line)
			if lastKey != "" && strings.HasPrefix(t, "- ") {
				list, _ := out[lastKey].([]string)
				out[lastKey] = append(list, unquote(strings.TrimSpace(t[2:])))
			}
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		lastKey = k
		switch {
		case v == "":
			out[k] = []string{}
		case strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]"):
			var items []string
			for _, it := range strings.Split(v[1:len(v)-1], ",") {
				if it = unquote(strings.TrimSpace(it)); it != "" {
					items = append(items, it)
				}
			}
			out[k] = items
		default:
			out[k] = unquote(v)
		}
	}
	return out
}

func unquote(s string) string {
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		return s[1 : len(s)-1]
	}
	return s
}
