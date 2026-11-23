package syncer

import (
	"errors"
	"strings"
	"testing"
)

func mustStrategy(t *testing.T, name, path string) strategy {
	t.Helper()
	s, err := strategyFor(name, path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFileStrategy(t *testing.T) {
	s := mustStrategy(t, StrategyFile, ".claude/skills/x/SKILL.md")
	tests := []struct {
		name      string
		existing  string
		exists    bool
		content   string
		want      string
		unchanged bool
	}{
		{name: "create", content: "hello\n", want: "hello\n"},
		{name: "replace", existing: "old\n", exists: true, content: "new\n", want: "new\n"},
		{name: "idempotent", existing: "same\n", exists: true, content: "same\n", want: "same\n", unchanged: true},
		{name: "crlf checkout equals lf content", existing: "a\r\nb\r\n", exists: true, content: "a\nb\n", want: "a\r\nb\r\n", unchanged: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var existing []byte
			if tc.exists {
				existing = []byte(tc.existing)
			}
			out, _, err := s.Apply(existing, tc.exists, tc.content)
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tc.want {
				t.Fatalf("got %q want %q", out, tc.want)
			}
			if tc.unchanged && string(out) != tc.existing {
				t.Fatalf("expected unchanged bytes")
			}
			h1, _, _ := s.Owned(out)
			h2, _, _ := s.Owned([]byte(strings.ReplaceAll(string(out), "\r\n", "\n")))
			if h1 != h2 {
				t.Fatalf("owned hash must ignore CRLF")
			}
		})
	}
}

func TestManagedBlockApply(t *testing.T) {
	md := mustStrategy(t, StrategyManagedBlock, "AGENTS.md")
	tests := []struct {
		name     string
		existing *string
		content  string
		want     string
		wantErr  string
	}{
		{
			name: "new file", content: "# Team rules\n- be kind",
			want: "<!-- orchestrai:begin -->\n# Team rules\n- be kind\n<!-- orchestrai:end -->\n",
		},
		{
			name: "append to hand-written file", existing: ptr("# My notes\n\nKeep this.\n"), content: "rule\n",
			want: "# My notes\n\nKeep this.\n\n<!-- orchestrai:begin -->\nrule\n<!-- orchestrai:end -->\n",
		},
		{
			name: "append when no trailing newline", existing: ptr("mine"), content: "rule\n",
			want: "mine\n\n<!-- orchestrai:begin -->\nrule\n<!-- orchestrai:end -->\n",
		},
		{
			name:     "replace block, keep text around it",
			existing: ptr("top\n<!-- orchestrai:begin -->\nold\n<!-- orchestrai:end -->\nbottom\n"),
			content:  "new1\nnew2\n",
			want:     "top\n<!-- orchestrai:begin -->\nnew1\nnew2\n<!-- orchestrai:end -->\nbottom\n",
		},
		{
			name:     "idempotent",
			existing: ptr("top\n<!-- orchestrai:begin -->\nsame\n<!-- orchestrai:end -->\n"),
			content:  "same",
			want:     "top\n<!-- orchestrai:begin -->\nsame\n<!-- orchestrai:end -->\n",
		},
		{
			name:     "crlf file keeps crlf",
			existing: ptr("top\r\n<!-- orchestrai:begin -->\r\nold\r\n<!-- orchestrai:end -->\r\nbottom\r\n"),
			content:  "a\nb\n",
			want:     "top\r\n<!-- orchestrai:begin -->\r\na\r\nb\r\n<!-- orchestrai:end -->\r\nbottom\r\n",
		},
		{
			name:     "crlf file append uses crlf",
			existing: ptr("mine\r\n"),
			content:  "x\n",
			want:     "mine\r\n\r\n<!-- orchestrai:begin -->\r\nx\r\n<!-- orchestrai:end -->\r\n",
		},
		{
			name:     "content already wrapped in markers",
			existing: ptr("top\n"),
			content:  "<!-- orchestrai:begin -->\ninner\n<!-- orchestrai:end -->\n",
			want:     "top\n\n<!-- orchestrai:begin -->\ninner\n<!-- orchestrai:end -->\n",
		},
		{
			name:     "indented markers still found",
			existing: ptr("  <!-- orchestrai:begin -->  \nold\n<!-- orchestrai:end -->\n"),
			content:  "new\n",
			want:     "<!-- orchestrai:begin -->\nnew\n<!-- orchestrai:end -->\n",
		},
		{
			name:     "end marker at EOF without newline",
			existing: ptr("<!-- orchestrai:begin -->\nold\n<!-- orchestrai:end -->"),
			content:  "new\n",
			want:     "<!-- orchestrai:begin -->\nnew\n<!-- orchestrai:end -->",
		},
		{name: "missing end marker", existing: ptr("<!-- orchestrai:begin -->\nhalf\n"), content: "x", wantErr: "without a matching"},
		{name: "missing begin marker", existing: ptr("text\n<!-- orchestrai:end -->\n"), content: "x", wantErr: "without a preceding"},
		{name: "reversed markers", existing: ptr("<!-- orchestrai:end -->\n<!-- orchestrai:begin -->\n"), content: "x", wantErr: "before the begin"},
		{
			name:     "two blocks",
			existing: ptr("<!-- orchestrai:begin -->\na\n<!-- orchestrai:end -->\n<!-- orchestrai:begin -->\nb\n<!-- orchestrai:end -->\n"),
			content:  "x", wantErr: "exactly one",
		},
		{name: "nested markers in bundle", content: "a\n<!-- orchestrai:end -->\nb", wantErr: "nested"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var existing []byte
			exists := tc.existing != nil
			if exists {
				existing = []byte(*tc.existing)
			}
			out, _, err := md.Apply(existing, exists, tc.content)
			if tc.wantErr != "" {
				var ce *ContentError
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !errors.As(err, &ce) {
					t.Fatalf("want ContentError containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tc.want {
				t.Fatalf("got\n%q\nwant\n%q", out, tc.want)
			}
			// Applying again is a no-op.
			again, _, err := md.Apply(out, true, tc.content)
			if err != nil || string(again) != string(out) {
				t.Fatalf("not idempotent: %q / %v", again, err)
			}
		})
	}
}

func TestManagedBlockTOMLAndRemove(t *testing.T) {
	toml := mustStrategy(t, StrategyManagedBlock, ".codex/config.toml")
	existing := "model = \"o4\"\n\n[profiles.x]\nfoo = 1\n"
	out, _, err := toml.Apply([]byte(existing), true, "[mcp_servers.team]\ncommand = \"npx\"\n")
	if err != nil {
		t.Fatal(err)
	}
	want := existing + "\n# orchestrai:begin\n[mcp_servers.team]\ncommand = \"npx\"\n# orchestrai:end\n"
	if string(out) != want {
		t.Fatalf("got %q", out)
	}
	h, found, err := toml.Owned(out)
	if err != nil || !found || h != SHA256Hex([]byte("[mcp_servers.team]\ncommand = \"npx\"\n")) {
		t.Fatalf("owned hash mismatch: %v %v", found, err)
	}
	removed, del, err := toml.Remove(out)
	if err != nil || del {
		t.Fatalf("remove: %v del=%v", err, del)
	}
	if string(removed) != existing {
		t.Fatalf("remove should restore the hand-written file, got %q", removed)
	}
	// A file that only held the block is deleted.
	onlyBlock, _, _ := toml.Apply(nil, false, "x = 1\n")
	if _, del, _ := toml.Remove(onlyBlock); !del {
		t.Fatal("expected delete of block-only file")
	}
	// Middle-of-file block removal keeps both sides.
	mid := "a\n<!-- orchestrai:begin -->\nx\n<!-- orchestrai:end -->\nb\n"
	md := mustStrategy(t, StrategyManagedBlock, "CLAUDE.md")
	got, del, err := md.Remove([]byte(mid))
	if err != nil || del || string(got) != "a\nb\n" {
		t.Fatalf("got %q del=%v err=%v", got, del, err)
	}
}

func TestJSONMerge(t *testing.T) {
	jm := mustStrategy(t, StrategyJSONMerge, ".mcp.json")
	bundle := `{"mcpServers":{"team-db":{"command":"npx","args":["db-mcp"],"x-orchestrai":true},"docs":{"url":"https://example.test/mcp"}}}`
	tests := []struct {
		name      string
		existing  *string
		content   string
		want      string
		wantWarn  string
		wantErr   string
		unchanged bool
	}{
		{
			name: "create", content: bundle,
			want: "{\n  \"mcpServers\": {\n    \"team-db\": {\n      \"command\": \"npx\",\n      \"args\": [\n        \"db-mcp\"\n      ],\n      \"x-orchestrai\": true\n    },\n    \"docs\": {\n      \"url\": \"https://example.test/mcp\",\n      \"x-orchestrai\": true\n    }\n  }\n}\n",
		},
		{
			name:     "keep user servers and other keys, drop stale ours",
			existing: ptr("{\n    \"mine\": 1,\n    \"mcpServers\": {\n        \"personal\": {\"command\": \"me\"},\n        \"old-team\": {\"command\": \"gone\", \"x-orchestrai\": true}\n    }\n}\n"),
			content:  `{"mcpServers":{"team":{"command":"t","x-orchestrai":true}}}`,
			want:     "{\n    \"mine\": 1,\n    \"mcpServers\": {\n        \"personal\": {\n            \"command\": \"me\"\n        },\n        \"team\": {\n            \"command\": \"t\",\n            \"x-orchestrai\": true\n        }\n    }\n}\n",
		},
		{
			name:     "user entry with same name wins",
			existing: ptr(`{"mcpServers":{"team":{"command":"mine"}}}`),
			content:  `{"mcpServers":{"team":{"command":"t"}}}`,
			wantWarn: "kept your own mcpServers.team", unchanged: true,
		},
		{
			name:      "idempotent keeps formatting",
			existing:  ptr("{\"mcpServers\":{\"team\":{\"x-orchestrai\":true,\"command\":\"t\"}}}"),
			content:   `{"mcpServers":{"team":{"command":"t","x-orchestrai":true}}}`,
			unchanged: true,
		},
		{
			name:     "vscode servers shape",
			existing: ptr("{\n\t\"servers\": {\n\t\t\"local\": {\"type\": \"stdio\", \"command\": \"x\"}\n\t}\n}\n"),
			content:  `{"servers":{"team":{"type":"http","url":"https://api.test/mcp/gateway/p1","x-orchestrai":true}}}`,
			want:     "{\n\t\"servers\": {\n\t\t\"local\": {\n\t\t\t\"type\": \"stdio\",\n\t\t\t\"command\": \"x\"\n\t\t},\n\t\t\"team\": {\n\t\t\t\"type\": \"http\",\n\t\t\t\"url\": \"https://api.test/mcp/gateway/p1\",\n\t\t\t\"x-orchestrai\": true\n\t\t}\n\t}\n}\n",
		},
		{
			name:     "crlf preserved",
			existing: ptr("{\r\n  \"mcpServers\": {}\r\n}\r\n"),
			content:  `{"mcpServers":{"a":{"command":"c"}}}`,
			want:     "{\r\n  \"mcpServers\": {\r\n    \"a\": {\r\n      \"command\": \"c\",\r\n      \"x-orchestrai\": true\r\n    }\r\n  }\r\n}\r\n",
		},
		{
			name: "html characters not escaped", content: `{"mcpServers":{"a":{"url":"https://x.test/?a=1&b=<2>"}}}`,
			want: "{\n  \"mcpServers\": {\n    \"a\": {\n      \"url\": \"https://x.test/?a=1&b=<2>\",\n      \"x-orchestrai\": true\n    }\n  }\n}\n",
		},
		{name: "malformed existing", existing: ptr(`{"mcpServers": {`), content: bundle, wantErr: "invalid JSON"},
		{name: "jsonc comments refused", existing: ptr("{\n  // mine\n  \"mcpServers\": {}\n}\n"), content: bundle, wantErr: "comments"},
		{name: "trailing data refused", existing: ptr(`{} {}`), content: bundle, wantErr: "after the top-level"},
		{name: "top-level array refused", existing: ptr(`[]`), content: bundle, wantErr: "not an object"},
		{name: "container not object", existing: ptr(`{"mcpServers": []}`), content: bundle, wantErr: `"mcpServers" is not an object`},
		{name: "bundle without container", content: `{"other":{}}`, wantErr: "neither"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var existing []byte
			exists := tc.existing != nil
			if exists {
				existing = []byte(*tc.existing)
			}
			out, warns, err := jm.Apply(existing, exists, tc.content)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.unchanged {
				if string(out) != *tc.existing {
					t.Fatalf("expected file untouched, got %q", out)
				}
			} else if string(out) != tc.want {
				t.Fatalf("got\n%s\nwant\n%s", out, tc.want)
			}
			if tc.wantWarn != "" && (len(warns) == 0 || !strings.Contains(warns[0], tc.wantWarn)) {
				t.Fatalf("want warning %q, got %v", tc.wantWarn, warns)
			}
			again, _, err := jm.Apply(out, true, tc.content)
			if err != nil || string(again) != string(out) {
				t.Fatalf("not idempotent")
			}
		})
	}
}

func TestJSONMergeOwnedAndRemove(t *testing.T) {
	jm := mustStrategy(t, StrategyJSONMerge, ".cursor/mcp.json")
	out, _, err := jm.Apply([]byte(`{"mcpServers":{"mine":{"command":"m"}}}`), true, `{"mcpServers":{"t":{"command":"t"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	h, found, err := jm.Owned(out)
	if err != nil || !found {
		t.Fatalf("owned: %v %v", found, err)
	}
	if h != SHA256Hex([]byte(`{"mcpServers":{"t":{"command":"t","x-orchestrai":true}}}`)) {
		t.Fatalf("canonical hash definition changed: %s", h)
	}
	// Reformatting must not change the owned hash.
	reformatted := strings.ReplaceAll(string(out), "\n", "\r\n")
	if h2, _, _ := jm.Owned([]byte(reformatted)); h2 != h {
		t.Fatal("owned hash must not depend on formatting")
	}
	removed, del, err := jm.Remove(out)
	if err != nil || del {
		t.Fatalf("remove: %v %v", err, del)
	}
	if strings.Contains(string(removed), `"t"`) || !strings.Contains(string(removed), `"mine"`) {
		t.Fatalf("remove must keep user entries only: %s", removed)
	}
	created, _, _ := jm.Apply(nil, false, `{"mcpServers":{"t":{"command":"t"}}}`)
	if _, del, _ := jm.Remove(created); !del {
		t.Fatal("a file holding only our servers should be deleted")
	}
}

func ptr(s string) *string { return &s }
