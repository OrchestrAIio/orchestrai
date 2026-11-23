package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/OrchestrAIio/orchestrai/cli/internal/creds"
)

var testKey = "ork_" + "test_0123456789abcdefghij"

type bundleFile struct {
	Path     string `json:"path"`
	Tool     string `json:"tool"`
	Strategy string `json:"strategy"`
	Content  string `json:"content"`
	SHA256   string `json:"sha256"`
}

// fakeBackend implements the backend-go routes the CLI calls.
type fakeBackend struct {
	t   *testing.T
	srv *httptest.Server

	mu            sync.Mutex
	version       int
	files         []bundleFile
	bundleLimit   bool // respond 402 to bundle requests
	chatLimit     bool
	bundleGets    int
	notModified   int
	reports       []map[string]any
	installs      []map[string]any
	published     []map[string]any
	revoked       []string
	execPolls     int
	messages      []map[string]string
	pollsUntilOK  int
	wsConns       chan *websocket.Conn
	workflowInput map[string]any
	// projects, when projectsSet, replaces the default p1/p2 list.
	projects      []map[string]any
	projectsSet   bool
	createdSpaces []string
	deviceNames   []string
	publicItems   map[string]map[string]any
	publicAuthHdr []string
	auditSummary  map[string]any
}

// setProjects makes GET /projects return exactly these team spaces.
func (fb *fakeBackend) setProjects(ps ...map[string]any) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.projects, fb.projectsSet = ps, true
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func newFakeBackend(t *testing.T) *fakeBackend {
	t.Helper()
	fb := &fakeBackend{t: t, version: 1, pollsUntilOK: 1, wsConns: make(chan *websocket.Conn, 4)}
	fb.setFiles(defaultBundleFiles())
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+testKey {
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"error":{"code":"UNAUTHORIZED","message":"Invalid API key"}}`))
				return
			}
			h(w, r)
		}
	}
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("POST /api/v1/editor/connect/start", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.deviceNames = append(fb.deviceNames, body["device_name"])
		fb.mu.Unlock()
		writeJSON(w, 200, map[string]any{"device_code": "dc", "user_code": "WXYZ-1234", "verification_uri": fb.srv.URL + "/live/connect", "verification_uri_complete": fb.srv.URL + "/live/connect?code=WXYZ-1234", "interval": 1, "expires_in": 60})
	})
	mux.HandleFunc("POST /api/v1/editor/connect/poll", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		defer fb.mu.Unlock()
		if fb.pollsUntilOK > 0 {
			fb.pollsUntilOK--
			writeJSON(w, 428, map[string]string{"error": "authorization_pending"})
			return
		}
		writeJSON(w, 200, map[string]any{"api_key": testKey, "user": map[string]string{"id": "u1", "name": "Ada Lovelace", "email": "ada@example.test"}})
	})
	mux.HandleFunc("GET /api/v1/projects", auth(func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		defer fb.mu.Unlock()
		projects := []map[string]any{{"id": "p1", "name": "Platform"}, {"id": "p2", "name": "Data"}}
		if fb.projectsSet {
			projects = append([]map[string]any{}, fb.projects...)
		}
		writeJSON(w, 200, map[string]any{"projects": projects, "total": len(projects), "page": 1, "page_size": 100})
	}))
	mux.HandleFunc("POST /api/v1/live/spaces", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		defer fb.mu.Unlock()
		fb.createdSpaces = append(fb.createdSpaces, body["name"])
		p := map[string]any{"id": fmt.Sprintf("p-new-%d", len(fb.createdSpaces)), "name": body["name"]}
		fb.projects, fb.projectsSet = append(fb.projects, p), true
		writeJSON(w, 201, map[string]any{"project": p})
	}))
	mux.HandleFunc("GET /api/v1/projects/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "p1" {
			writeJSON(w, 404, map[string]string{"error": "Project not found"})
			return
		}
		writeJSON(w, 200, map[string]any{"id": "p1", "name": "Platform"})
	}))
	mux.HandleFunc("GET /api/v1/api-keys", auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, []map[string]any{{"id": "k1", "name": "OrchestrAI Live — ork", "key_prefix": testKey[:12]}})
	}))
	mux.HandleFunc("DELETE /api/v1/api-keys/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.revoked = append(fb.revoked, r.PathValue("id"))
		fb.mu.Unlock()
		writeJSON(w, 200, map[string]string{"message": "API key revoked"})
	}))
	mux.HandleFunc("GET /api/v1/projects/{id}/context/bundle", auth(func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		defer fb.mu.Unlock()
		fb.bundleGets++
		if fb.bundleLimit {
			writeJSON(w, 402, map[string]any{"error": "The free plan includes 1 synced repo.", "code": "LIVE_LIMIT_HUB_REPOS", "cta": "upgrade", "limit": 1, "current": 1, "upgrade_url": "https://app.test/settings/billing?from=live_hub_repos"})
			return
		}
		etag := fmt.Sprintf(`"v%d"`, fb.version)
		if r.Header.Get("If-None-Match") == etag {
			fb.notModified++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		writeJSON(w, 200, map[string]any{"version": fb.version, "hash": fmt.Sprintf("hash-%d", fb.version), "rules": []any{}, "skills": []any{}, "docs": []any{}, "mcp_servers": []any{}, "files": fb.files})
	}))
	mux.HandleFunc("POST /api/v1/projects/{id}/context/sync-reports", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.reports = append(fb.reports, body)
		fb.mu.Unlock()
		w.WriteHeader(201)
	}))
	// Library.
	reviewItem := map[string]any{
		"id": "item-1", "publisher": "acme", "slug": "pr-review", "name": "PR Review",
		"kind": "skill", "scope": "community", "latest_version": "1.2.0", "description": "Reviews PRs",
		"files": []map[string]any{
			{"path": "SKILL.md", "content": "---\nname: PR Review\ndescription: Reviews PRs\n---\nReview carefully.\n"},
		},
	}
	fb.publicItems = map[string]map[string]any{
		"acme/pr-review": reviewItem,
	}
	servePublicItem := func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.publicAuthHdr = append(fb.publicAuthHdr, r.Header.Get("Authorization"))
		it, ok := fb.publicItems[r.PathValue("pub")+"/"+r.PathValue("slug")]
		fb.mu.Unlock()
		if ok {
			writeJSON(w, 200, map[string]any{"item": it})
			return
		}
		writeJSON(w, 404, map[string]string{"error": "not found"})
	}
	mux.HandleFunc("GET /api/v1/public/library/items/{pub}/{slug}", servePublicItem)
	mux.HandleFunc("GET /api/v1/public/library/items/{pub}/{slug}/versions/{ver}", servePublicItem)
	mux.HandleFunc("GET /api/v1/library/items/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "item-1" {
			writeJSON(w, 200, map[string]any{"item": reviewItem})
			return
		}
		writeJSON(w, 404, map[string]string{"error": "not found"})
	}))
	mux.HandleFunc("GET /api/v1/projects/{id}/live/audit-summary", auth(func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		sum := fb.auditSummary
		fb.mu.Unlock()
		if sum == nil {
			sum = map[string]any{"connected": false}
		}
		writeJSON(w, 200, sum)
	}))
	mux.HandleFunc("GET /api/v1/library/items", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("scope") == "" {
			writeJSON(w, 400, map[string]string{"error": "scope required"})
			return
		}
		writeJSON(w, 200, map[string]any{"items": []any{reviewItem}})
	}))
	mux.HandleFunc("POST /api/v1/library/items", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.published = append(fb.published, body)
		fb.mu.Unlock()
		writeJSON(w, 201, map[string]any{"item": map[string]any{"id": "item-9", "publisher": "ada", "slug": body["slug"], "name": body["name"], "kind": body["kind"], "latest_version": "1.0.0"}})
	}))
	mux.HandleFunc("POST /api/v1/projects/{id}/library/installs", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		defer fb.mu.Unlock()
		inst := map[string]any{"id": "inst-1", "item_id": body["item_id"], "version": "1.2.0", "item": reviewItem}
		fb.installs = append(fb.installs, inst)
		fb.files = append(fb.files, mkFile(".claude/skills/pr-review/SKILL.md", "claude", "file", "---\nname: PR Review\n---\nReview carefully.\n"))
		fb.version++
		writeJSON(w, 201, map[string]any{"install": inst})
	}))
	mux.HandleFunc("GET /api/v1/projects/{id}/library/installs", auth(func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		defer fb.mu.Unlock()
		writeJSON(w, 200, map[string]any{"installs": fb.installs})
	}))
	mux.HandleFunc("DELETE /api/v1/projects/{id}/library/installs/{inst}", auth(func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		defer fb.mu.Unlock()
		fb.installs = nil
		var kept []bundleFile
		for _, f := range fb.files {
			if !strings.HasPrefix(f.Path, ".claude/skills/pr-review/") {
				kept = append(kept, f)
			}
		}
		fb.files = kept
		fb.version++
		w.WriteHeader(204)
	}))
	// Workflows.
	mux.HandleFunc("GET /api/v1/projects/{id}/workflows/definitions", auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"workflows": []map[string]any{{"id": "wf-1", "name": "Nightly backup", "is_active": true, "version": 3}}, "count": 1})
	}))
	mux.HandleFunc("POST /api/v1/projects/{id}/workflows/definitions/{wid}/execute", auth(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input map[string]any `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.workflowInput = body.Input
		fb.mu.Unlock()
		writeJSON(w, 202, map[string]any{"id": "ex-1", "workflow_id": r.PathValue("wid"), "status": "pending"})
	}))
	mux.HandleFunc("GET /api/v1/projects/{id}/workflows/executions/{eid}", auth(func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.execPolls++
		n := fb.execPolls
		fb.mu.Unlock()
		status, node := "running", "running"
		if n >= 2 {
			status, node = "completed", "completed"
		}
		writeJSON(w, 200, map[string]any{"id": "ex-1", "status": status, "node_executions": map[string]any{"backup": map[string]string{"status": node}}, "output_payload": map[string]any{"ok": true}})
	}))
	// Rooms.
	mux.HandleFunc("GET /api/v1/projects/{id}/rooms", auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"rooms": []map[string]any{{"id": "room-1", "project_id": "p1", "title": "Pairing", "status": "live", "peer_count": 2}}})
	}))
	mux.HandleFunc("GET /api/v1/rooms/{rid}", auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"room": map[string]any{"id": r.PathValue("rid"), "project_id": "p1", "title": "Pairing", "status": "live"}})
	}))
	// Chat + WebSocket.
	mux.HandleFunc("POST /api/v1/ws/ticket", auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ticket": "tkt", "expires_in": 60})
	}))
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ticket") != "tkt" || r.URL.Query().Get("project_id") != "p1" {
			w.WriteHeader(401)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		fb.wsConns <- c
		// Keep the handler alive until the client disconnects.
		_, _, _ = c.Read(context.Background())
	})
	mux.HandleFunc("POST /api/v1/projects/{id}/conversations", auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 201, map[string]any{"id": "conv-1", "title": "t"})
	}))
	mux.HandleFunc("GET /api/v1/projects/{id}/conversations/{cid}/messages", auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"messages": []map[string]string{{"role": "user", "content": "earlier"}, {"role": "assistant", "content": "answer"}}})
	}))
	mux.HandleFunc("POST /api/v1/projects/{id}/conversations/{cid}/messages", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.messages = append(fb.messages, body)
		fb.mu.Unlock()
		writeJSON(w, 201, map[string]any{"id": fmt.Sprintf("m%d", len(fb.messages)), "role": body["role"], "content": body["content"]})
	}))
	mux.HandleFunc("POST /api/v1/projects/{id}/chat/message", auth(func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		limited := fb.chatLimit
		fb.mu.Unlock()
		if limited {
			writeJSON(w, 402, map[string]any{"error": "You are out of credits.", "code": "INSUFFICIENT_CREDITS", "cta": "buy_credits", "buy_credits_url": "https://app.test/settings/billing?from=live_cli_ask", "upgrade_url": "https://app.test/pricing"})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		conv, _ := body["conversation_id"].(string)
		var c *websocket.Conn
		select {
		case c = <-fb.wsConns:
		case <-time.After(5 * time.Second):
			t.Error("no websocket connected before chat/message")
			w.WriteHeader(500)
			return
		}
		go func() {
			ctx := context.Background()
			send := func(v any) {
				b, _ := json.Marshal(v)
				_ = c.Write(ctx, websocket.MessageText, b)
			}
			send(map[string]any{"type": "tokens", "run_id": "someone-else", "data": map[string]any{"run_id": "someone-else", "tokens": map[string]any{"delta": "IGNORED"}}})
			send(map[string]any{"type": "tokens", "run_id": conv, "data": map[string]any{"run_id": conv, "tokens": map[string]any{"delta": "Hello "}}})
			send(map[string]any{"type": "tokens", "run_id": conv, "data": map[string]any{"run_id": conv, "tokens": map[string]any{"delta": "team!", "finish_reason": "stop"}}})
		}()
		writeJSON(w, 202, map[string]any{"success": true, "conversation_id": conv})
	}))
	fb.srv = httptest.NewServer(mux)
	t.Cleanup(fb.srv.Close)
	return fb
}

func mkFile(path, tool, strategy, content string) bundleFile {
	return bundleFile{Path: path, Tool: tool, Strategy: strategy, Content: content, SHA256: sha(content)}
}

func defaultBundleFiles() []bundleFile {
	return []bundleFile{
		mkFile("AGENTS.md", "agents", "managed_block", "## Team rules\n\n- Use pnpm, never npm.\n- Every PR needs a test.\n"),
		mkFile("CLAUDE.md", "claude", "managed_block", "@AGENTS.md\n"),
		mkFile(".mcp.json", "claude", "json_merge", `{"mcpServers":{"orchestrai":{"type":"http","url":"https://api.test/mcp/gateway/p1","x-orchestrai":true},"github":{"command":"npx","args":["-y","@modelcontextprotocol/server-github"],"env":{"GITHUB_TOKEN":"${GITHUB_TOKEN}"},"x-orchestrai":true}}}`),
		mkFile(".cursor/mcp.json", "cursor", "json_merge", `{"mcpServers":{"orchestrai":{"url":"https://api.test/mcp/gateway/p1","x-orchestrai":true}}}`),
		mkFile(".vscode/mcp.json", "vscode", "json_merge", `{"servers":{"orchestrai":{"type":"http","url":"https://api.test/mcp/gateway/p1","x-orchestrai":true}}}`),
		mkFile(".codex/config.toml", "codex", "managed_block", "[mcp_servers.orchestrai]\nurl = \"https://api.test/mcp/gateway/p1\"\n"),
		mkFile(".cursor/rules/backend.mdc", "cursor", "file", "---\nglobs: src/api/**\n---\nValidate input with zod.\n"),
		mkFile(".github/instructions/backend.instructions.md", "copilot", "file", "---\napplyTo: \"src/api/**\"\n---\nValidate input with zod.\n"),
	}
}

func (fb *fakeBackend) setFiles(files []bundleFile) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.files = files
}

func (fb *fakeBackend) bump(mutate func(files []bundleFile) []bundleFile) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.files = mutate(fb.files)
	fb.version++
}

// testApp builds an App running in dir against fb with file-based creds.
type testApp struct {
	*App
	out, errOut *bytes.Buffer
	opened      []string
}

func newTestApp(t *testing.T, fb *fakeBackend, dir string) *testApp {
	t.Helper()
	t.Setenv(creds.EnvAPIURL, fb.srv.URL)
	t.Setenv(creds.EnvAPIKey, "")
	t.Setenv(EnvProjectID, "")
	t.Setenv(EnvAppURL, "https://app.test")
	t.Setenv(EnvWSURL, "")
	ta := &testApp{out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	ta.App = &App{
		Prog: "orchestrai", Stdin: strings.NewReader(""), Stdout: ta.out, Stderr: ta.errOut, Dir: dir,
		Store: &creds.Store{Dir: filepath.Join(t.TempDir(), "config")},
		HTTP:  fb.srv.Client(),
		Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() },
		Now:   time.Now,
		IsTTY: func() bool { return false },
	}
	ta.App.OpenURL = func(u string) error { ta.opened = append(ta.opened, u); return nil }
	return ta
}

func (ta *testApp) run(t *testing.T, args ...string) int {
	t.Helper()
	ta.out.Reset()
	ta.errOut.Reset()
	return ta.Run(context.Background(), args)
}

func (ta *testApp) login(t *testing.T) {
	t.Helper()
	if code := ta.run(t, "login", "--no-browser", "--device-name", "ci"); code != 0 {
		t.Fatalf("login exit %d: %s", code, ta.errOut)
	}
}

func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
