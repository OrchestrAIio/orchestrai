package syncer

import (
	"bytes"
	"fmt"
	"strings"
)

// Markers for managed blocks. TOML files (Codex config.toml) use comment
// markers that TOML parsers ignore.
const (
	MDBegin   = "<!-- orchestrai:begin -->"
	MDEnd     = "<!-- orchestrai:end -->"
	TOMLBegin = "# orchestrai:begin"
	TOMLEnd   = "# orchestrai:end"
)

// ContentError is local or bundle content the CLI refuses to modify
// (malformed markers, invalid JSON, wrong shapes).
type ContentError struct {
	Path   string
	Reason string
}

func (e *ContentError) Error() string {
	return fmt.Sprintf("refusing to modify %s: %s", e.Path, e.Reason)
}

// strategy applies one bundle file to the current file content.
type strategy interface {
	// Apply returns the new file content (existing is nil when absent) and
	// warnings. It returns existing unchanged when nothing needs to change.
	Apply(existing []byte, exists bool, content string) ([]byte, []string, error)
	// Owned returns the hash of the portion of current that the CLI owns
	// (see manifest.go for the exact definition).
	Owned(current []byte) (hash string, found bool, err error)
	// Remove strips the owned portion. deleteFile is true when nothing but
	// whitespace (or an empty server map) would remain.
	Remove(current []byte) (out []byte, deleteFile bool, err error)
}

func strategyFor(name, relPath string) (strategy, error) {
	switch name {
	case StrategyFile:
		return fileStrategy{}, nil
	case StrategyManagedBlock:
		if strings.HasSuffix(strings.ToLower(relPath), ".toml") {
			return managedBlock{path: relPath, begin: TOMLBegin, end: TOMLEnd}, nil
		}
		return managedBlock{path: relPath, begin: MDBegin, end: MDEnd}, nil
	case StrategyJSONMerge:
		return jsonMerge{path: relPath}, nil
	}
	return nil, fmt.Errorf("%s: unknown strategy %q", relPath, name)
}

// fileStrategy owns the whole file.
type fileStrategy struct{}

func (fileStrategy) Apply(existing []byte, exists bool, content string) ([]byte, []string, error) {
	if exists && bytes.Equal(NormalizeLF(existing), NormalizeLF([]byte(content))) {
		return existing, nil, nil
	}
	return []byte(content), nil, nil
}

func (fileStrategy) Owned(current []byte) (string, bool, error) {
	return SHA256Hex(NormalizeLF(current)), true, nil
}

func (fileStrategy) Remove([]byte) ([]byte, bool, error) { return nil, true, nil }

// managedBlock owns the text between two marker lines.
type managedBlock struct {
	path, begin, end string
}

type blockLoc struct {
	start, innerStart, innerEnd, stop int
	endHasNewline                     bool
}

// locate finds exactly one begin…end pair. Zero markers → found=false.
func (m managedBlock) locate(b []byte) (blockLoc, bool, error) {
	var begins, ends []blockLoc
	for off := 0; off < len(b); {
		nl := bytes.IndexByte(b[off:], '\n')
		lineEnd, next := len(b), len(b)
		if nl >= 0 {
			lineEnd, next = off+nl, off+nl+1
		}
		line := strings.TrimSpace(string(b[off:lineEnd]))
		switch line {
		case m.begin:
			begins = append(begins, blockLoc{start: off, innerStart: next})
		case m.end:
			ends = append(ends, blockLoc{innerEnd: off, stop: next, endHasNewline: nl >= 0})
		}
		off = next
	}
	switch {
	case len(begins) == 0 && len(ends) == 0:
		return blockLoc{}, false, nil
	case len(begins) == 1 && len(ends) == 1 && begins[0].innerStart <= ends[0].innerEnd:
		return blockLoc{
			start: begins[0].start, innerStart: begins[0].innerStart,
			innerEnd: ends[0].innerEnd, stop: ends[0].stop, endHasNewline: ends[0].endHasNewline,
		}, true, nil
	case len(begins) == 1 && len(ends) == 0:
		return blockLoc{}, false, &ContentError{Path: m.path, Reason: fmt.Sprintf("found %q without a matching %q; restore the end marker or delete the block by hand", m.begin, m.end)}
	case len(begins) == 0 && len(ends) == 1:
		return blockLoc{}, false, &ContentError{Path: m.path, Reason: fmt.Sprintf("found %q without a preceding %q; fix the markers by hand", m.end, m.begin)}
	case len(begins) == 1 && len(ends) == 1:
		return blockLoc{}, false, &ContentError{Path: m.path, Reason: "the end marker comes before the begin marker; fix the markers by hand"}
	}
	return blockLoc{}, false, &ContentError{Path: m.path, Reason: fmt.Sprintf("found %d begin and %d end markers; keep exactly one orchestrai block", len(begins), len(ends))}
}

// body normalizes bundle content into the inner block text (LF, trailing
// newline). Content that already carries the markers is unwrapped.
func (m managedBlock) body(content string) (string, error) {
	c := string(NormalizeLF([]byte(content)))
	lines := strings.Split(strings.TrimRight(c, "\n"), "\n")
	if len(lines) >= 2 && strings.TrimSpace(lines[0]) == m.begin && strings.TrimSpace(lines[len(lines)-1]) == m.end {
		c = strings.Join(lines[1:len(lines)-1], "\n")
	}
	for _, l := range strings.Split(c, "\n") {
		if t := strings.TrimSpace(l); t == m.begin || t == m.end {
			return "", &ContentError{Path: m.path, Reason: "bundle content contains nested orchestrai markers"}
		}
	}
	if c != "" && !strings.HasSuffix(c, "\n") {
		c += "\n"
	}
	return c, nil
}

func (m managedBlock) Apply(existing []byte, exists bool, content string) ([]byte, []string, error) {
	body, err := m.body(content)
	if err != nil {
		return nil, nil, err
	}
	if !exists || len(bytes.TrimSpace(existing)) == 0 {
		return []byte(m.begin + "\n" + body + m.end + "\n"), nil, nil
	}
	loc, found, err := m.locate(existing)
	if err != nil {
		return nil, nil, err
	}
	eol := DetectEOL(existing)
	if found {
		if string(NormalizeLF(existing[loc.innerStart:loc.innerEnd])) == body {
			return existing, nil, nil
		}
		var out bytes.Buffer
		out.Write(existing[:loc.start])
		out.WriteString(m.begin + eol)
		out.Write(toEOL([]byte(body), eol))
		out.WriteString(m.end)
		if loc.endHasNewline {
			out.WriteString(eol)
		}
		out.Write(existing[loc.stop:])
		return out.Bytes(), nil, nil
	}
	var out bytes.Buffer
	out.Write(existing)
	if !bytes.HasSuffix(existing, []byte("\n")) {
		out.WriteString(eol)
	}
	if !bytes.HasSuffix(NormalizeLF(out.Bytes()), []byte("\n\n")) {
		out.WriteString(eol)
	}
	out.WriteString(m.begin + eol)
	out.Write(toEOL([]byte(body), eol))
	out.WriteString(m.end + eol)
	return out.Bytes(), nil, nil
}

func (m managedBlock) Owned(current []byte) (string, bool, error) {
	loc, found, err := m.locate(current)
	if err != nil || !found {
		return "", false, err
	}
	return SHA256Hex(NormalizeLF(current[loc.innerStart:loc.innerEnd])), true, nil
}

func (m managedBlock) Remove(current []byte) ([]byte, bool, error) {
	loc, found, err := m.locate(current)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return current, len(bytes.TrimSpace(current)) == 0, nil
	}
	before := current[:loc.start]
	after := current[loc.stop:]
	var out []byte
	if len(after) == 0 {
		// Block was at the end: drop the blank separator line we added.
		eol := DetectEOL(current)
		trimmed := bytes.TrimRight(before, "\r\n")
		out = append([]byte(nil), trimmed...)
		if len(trimmed) > 0 {
			out = append(out, eol...)
		}
	} else {
		out = append(append([]byte(nil), before...), after...)
	}
	return out, len(bytes.TrimSpace(out)) == 0, nil
}
