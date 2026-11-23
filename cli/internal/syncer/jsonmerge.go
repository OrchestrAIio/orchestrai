package syncer

import (
	"bytes"
	"fmt"
)

// OwnershipKey marks MCP server entries that the CLI manages.
const OwnershipKey = "x-orchestrai"

// serverContainers are the MCP config shapes: VS Code uses "servers",
// Claude Code / Cursor use "mcpServers".
var serverContainers = []string{"mcpServers", "servers"}

// jsonMerge owns only the server entries flagged "x-orchestrai": true.
type jsonMerge struct{ path string }

func isOurs(v any) bool {
	o, ok := v.(*object)
	if !ok {
		return false
	}
	flag, ok := o.vals[OwnershipKey].(bool)
	return ok && flag
}

func (j jsonMerge) parseExisting(existing []byte) (*object, error) {
	if len(bytes.TrimSpace(existing)) == 0 {
		return newObject(), nil
	}
	v, err := parseOrdered(existing)
	if err != nil {
		return nil, &ContentError{Path: j.path, Reason: err.Error()}
	}
	o, ok := v.(*object)
	if !ok {
		return nil, &ContentError{Path: j.path, Reason: "the top-level JSON value is not an object"}
	}
	return o, nil
}

func (j jsonMerge) serialize(o *object, existing []byte, exists bool) []byte {
	indent, eol := "  ", "\n"
	if exists && len(bytes.TrimSpace(existing)) > 0 {
		indent, eol = detectIndent(existing), DetectEOL(existing)
	}
	var buf bytes.Buffer
	writeValue(&buf, o, indent, "")
	buf.WriteByte('\n')
	return toEOL(buf.Bytes(), eol)
}

func (j jsonMerge) Apply(existing []byte, exists bool, content string) ([]byte, []string, error) {
	dv, err := parseOrdered([]byte(content))
	if err != nil {
		return nil, nil, fmt.Errorf("bundle content for %s: %w", j.path, err)
	}
	desired, ok := dv.(*object)
	if !ok {
		return nil, nil, fmt.Errorf("bundle content for %s is not a JSON object", j.path)
	}
	var containers []string
	for _, k := range serverContainers {
		if _, ok := desired.get(k); ok {
			containers = append(containers, k)
		}
	}
	if len(containers) == 0 {
		return nil, nil, fmt.Errorf("bundle content for %s has neither %q nor %q", j.path, "mcpServers", "servers")
	}
	cur, err := j.parseExisting(existing)
	if err != nil {
		return nil, nil, err
	}
	before := canonical(cur)
	var warnings []string
	for _, k := range containers {
		dc, ok := desired.vals[k].(*object)
		if !ok {
			return nil, nil, fmt.Errorf("bundle content for %s: %q is not an object", j.path, k)
		}
		var ec *object
		if v, present := cur.get(k); present {
			if ec, ok = v.(*object); !ok {
				return nil, nil, &ContentError{Path: j.path, Reason: fmt.Sprintf("%q is not an object", k)}
			}
		} else {
			ec = newObject()
			cur.set(k, ec)
		}
		for _, name := range append([]string(nil), ec.keys...) {
			if _, keep := dc.get(name); !keep && isOurs(ec.vals[name]) {
				ec.del(name)
			}
		}
		for _, name := range dc.keys {
			entry, ok := dc.vals[name].(*object)
			if !ok {
				return nil, nil, fmt.Errorf("bundle content for %s: server %q is not an object", j.path, name)
			}
			entry.set(OwnershipKey, true)
			if old, present := ec.get(name); present && !isOurs(old) {
				warnings = append(warnings, fmt.Sprintf("%s: kept your own %s.%s; the team definition with the same name was not applied", j.path, k, name))
				continue
			}
			ec.set(name, entry)
		}
	}
	for _, k := range desired.keys {
		if isContainer(k) {
			continue
		}
		if _, present := cur.get(k); !present {
			cur.set(k, desired.vals[k])
		}
	}
	if exists && bytes.Equal(before, canonical(cur)) {
		return existing, warnings, nil
	}
	return j.serialize(cur, existing, exists), warnings, nil
}

func isContainer(k string) bool {
	for _, c := range serverContainers {
		if c == k {
			return true
		}
	}
	return false
}

// owned collects the flagged entries per container.
func (j jsonMerge) owned(o *object) *object {
	out := newObject()
	for _, k := range serverContainers {
		c, ok := o.vals[k].(*object)
		if !ok {
			continue
		}
		sub := newObject()
		for _, name := range c.keys {
			if isOurs(c.vals[name]) {
				sub.set(name, c.vals[name])
			}
		}
		if len(sub.keys) > 0 {
			out.set(k, sub)
		}
	}
	return out
}

func (j jsonMerge) Owned(current []byte) (string, bool, error) {
	cur, err := j.parseExisting(current)
	if err != nil {
		return "", false, err
	}
	o := j.owned(cur)
	if len(o.keys) == 0 {
		return "", false, nil
	}
	return SHA256Hex(canonical(o)), true, nil
}

func (j jsonMerge) Remove(current []byte) ([]byte, bool, error) {
	cur, err := j.parseExisting(current)
	if err != nil {
		return nil, false, err
	}
	changed := false
	for _, k := range serverContainers {
		c, ok := cur.vals[k].(*object)
		if !ok {
			continue
		}
		for _, name := range append([]string(nil), c.keys...) {
			if isOurs(c.vals[name]) {
				c.del(name)
				changed = true
			}
		}
	}
	onlyEmpty := true
	for _, k := range cur.keys {
		c, ok := cur.vals[k].(*object)
		if !isContainer(k) || !ok || len(c.keys) > 0 {
			onlyEmpty = false
			break
		}
	}
	if onlyEmpty {
		return nil, true, nil
	}
	if !changed {
		return current, false, nil
	}
	return j.serialize(cur, current, true), false, nil
}
