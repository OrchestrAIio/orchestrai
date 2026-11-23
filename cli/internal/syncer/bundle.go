// Package syncer applies a context bundle (spec §5 "Context hub") to a
// repository: path safety, the three apply strategies, the manifest, drift
// detection, deletion of dropped files, and unified diffs.
package syncer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Strategy names (spec §5).
const (
	StrategyFile         = "file"
	StrategyManagedBlock = "managed_block"
	StrategyJSONMerge    = "json_merge"
)

// FlexString decodes a JSON string or number into a string.
type FlexString string

// UnmarshalJSON accepts "1.2", 12, or null.
func (f *FlexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		*f = ""
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = FlexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("version must be a string or number: %w", err)
	}
	*f = FlexString(n.String())
	return nil
}

// File is one entry of bundle.files[].
type File struct {
	Path     string `json:"path"`
	Tool     string `json:"tool"`
	Strategy string `json:"strategy"`
	Content  string `json:"content"`
	SHA256   string `json:"sha256"`
}

// Bundle is the GET /context/bundle response (only what the CLI needs).
type Bundle struct {
	Version FlexString `json:"version"`
	Hash    string     `json:"hash"`
	Files   []File     `json:"files"`
	// Warnings name items the server left out (e.g. unsafe names).
	Warnings []string `json:"warnings,omitempty"`
}

// ParseBundle decodes and validates a bundle body.
func ParseBundle(data []byte) (*Bundle, error) {
	var b Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("decode context bundle: %w", err)
	}
	seen := map[string]bool{}
	for i, f := range b.Files {
		if f.Path == "" {
			return nil, fmt.Errorf("bundle file #%d has no path", i)
		}
		switch f.Strategy {
		case StrategyFile, StrategyManagedBlock, StrategyJSONMerge:
		default:
			return nil, fmt.Errorf("bundle file %s: unknown strategy %q", f.Path, f.Strategy)
		}
		if seen[f.Path] {
			return nil, fmt.Errorf("bundle lists %s twice", f.Path)
		}
		seen[f.Path] = true
		if f.SHA256 != "" {
			want := strings.ToLower(f.SHA256)
			if want != SHA256Hex([]byte(f.Content)) && want != SHA256Hex(NormalizeLF([]byte(f.Content))) {
				return nil, fmt.Errorf("bundle file %s: content does not match its sha256 (corrupted download?)", f.Path)
			}
		}
	}
	return &b, nil
}

// SHA256Hex returns the lowercase hex sha256 of b.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// NormalizeLF converts CRLF (and lone CR) line endings to LF.
func NormalizeLF(b []byte) []byte {
	if !bytes.Contains(b, []byte("\r")) {
		return b
	}
	out := bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(out, []byte("\r"), []byte("\n"))
}

// DetectEOL returns "\r\n" when most line breaks in b are CRLF.
func DetectEOL(b []byte) string {
	crlf := bytes.Count(b, []byte("\r\n"))
	lf := bytes.Count(b, []byte("\n"))
	if crlf > 0 && crlf*2 >= lf {
		return "\r\n"
	}
	return "\n"
}

// toEOL converts LF-normalized text to the given EOL.
func toEOL(lfText []byte, eol string) []byte {
	if eol == "\n" {
		return lfText
	}
	return bytes.ReplaceAll(lfText, []byte("\n"), []byte(eol))
}

// SharedTool reports whether a bundle tool value applies to every tool
// (AGENTS.md and other cross-tool files).
func SharedTool(tool string) bool {
	switch strings.ToLower(tool) {
	case "", "all", "agents", "shared", "common":
		return true
	}
	return false
}
