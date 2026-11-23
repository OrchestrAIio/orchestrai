package syncer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// object is a JSON object that remembers key order so rewritten config
// files keep the user's layout.
type object struct {
	keys []string
	vals map[string]any
}

func newObject() *object { return &object{vals: map[string]any{}} }

func (o *object) get(k string) (any, bool) {
	v, ok := o.vals[k]
	return v, ok
}

func (o *object) set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *object) del(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, kk := range o.keys {
		if kk == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// parseOrdered decodes JSON into *object / []any / string / json.Number /
// bool / nil, rejecting trailing data.
func parseOrdered(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, describeJSONError(data, dec, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("unexpected data after the top-level value (offset %d)", dec.InputOffset())
	}
	return v, nil
}

func describeJSONError(data []byte, dec *json.Decoder, err error) error {
	off := int(dec.InputOffset())
	var se *json.SyntaxError
	if errors.As(err, &se) {
		off = int(se.Offset)
	}
	if off > len(data) {
		off = len(data)
	}
	line := 1 + bytes.Count(data[:off], []byte("\n"))
	col := off - bytes.LastIndexByte(data[:off], '\n')
	msg := err.Error()
	if bytes.Contains(data, []byte("//")) || bytes.Contains(data, []byte("/*")) {
		msg += " (comments and trailing commas are not supported; remove them so the file can be merged)"
	}
	return fmt.Errorf("invalid JSON at line %d, column %d: %s", line, col, msg)
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := newObject()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("object key is not a string")
				}
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				o.set(k, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return o, nil
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	default:
		return t, nil
	}
}

func encodeString(buf *bytes.Buffer, s string) {
	var tmp bytes.Buffer
	enc := json.NewEncoder(&tmp)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	buf.Write(bytes.TrimRight(tmp.Bytes(), "\n"))
}

// writeValue serializes v. indent == "" produces compact output.
func writeValue(buf *bytes.Buffer, v any, indent, prefix string) {
	nl := func(p string) {
		if indent != "" {
			buf.WriteByte('\n')
			buf.WriteString(p)
		}
	}
	sep := ":"
	if indent != "" {
		sep = ": "
	}
	switch t := v.(type) {
	case *object:
		if len(t.keys) == 0 {
			buf.WriteString("{}")
			return
		}
		buf.WriteByte('{')
		for i, k := range t.keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			nl(prefix + indent)
			encodeString(buf, k)
			buf.WriteString(sep)
			writeValue(buf, t.vals[k], indent, prefix+indent)
		}
		nl(prefix)
		buf.WriteByte('}')
	case []any:
		if len(t) == 0 {
			buf.WriteString("[]")
			return
		}
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			nl(prefix + indent)
			writeValue(buf, e, indent, prefix+indent)
		}
		nl(prefix)
		buf.WriteByte(']')
	case string:
		encodeString(buf, t)
	case json.Number:
		buf.WriteString(t.String())
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case nil:
		buf.WriteString("null")
	default:
		// Values built in code (map/slice of plain Go types).
		b, _ := json.Marshal(t)
		buf.Write(b)
	}
}

// canonical serializes with sorted object keys and no whitespace; it is
// the hashing form for json_merge ownership (see manifest.go).
func canonical(v any) []byte {
	var buf bytes.Buffer
	writeCanonical(&buf, v)
	return buf.Bytes()
}

func writeCanonical(buf *bytes.Buffer, v any) {
	switch t := v.(type) {
	case *object:
		keys := append([]string(nil), t.keys...)
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			encodeString(buf, k)
			buf.WriteByte(':')
			writeCanonical(buf, t.vals[k])
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonical(buf, e)
		}
		buf.WriteByte(']')
	default:
		writeValue(buf, t, "", "")
	}
}

// detectIndent returns the indentation unit of a pretty-printed document
// (tab, or N spaces), defaulting to two spaces.
func detectIndent(data []byte) string {
	for _, line := range strings.Split(string(NormalizeLF(data)), "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || len(trimmed) == len(line) {
			continue
		}
		lead := line[:len(line)-len(trimmed)]
		if strings.HasPrefix(lead, "\t") {
			return "\t"
		}
		if n := len(lead); n >= 1 && n <= 8 {
			return strings.Repeat(" ", n)
		}
	}
	return "  "
}
