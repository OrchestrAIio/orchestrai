package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OrchestrAIio/orchestrai/cli/internal/creds"
)

func TestDetectLocalLLMKeysMasksSecrets(t *testing.T) {
	env := map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-api03-secretvalue1234",
		"GEMINI_API_KEY":    "AIzaSySecretGoogleKey9876",
		"OPENAI_API_KEY":    "short",
	}
	found := detectLocalLLMKeys(func(k string) string { return env[k] })
	if len(found) != 2 {
		t.Fatalf("expected 2 detected keys, got %+v", found)
	}
	if found[0].Provider != "anthropic" || found[0].Last4 != "1234" {
		t.Fatalf("unexpected first key: %+v", found[0])
	}
	if found[1].Provider != "google" || found[1].Last4 != "9876" {
		t.Fatalf("unexpected second key: %+v", found[1])
	}
}

func TestCmdKeysStatusAndOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/llm-keys" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{
				{
					"id":               "k1",
					"owner_type":       "user",
					"provider":         "anthropic",
					"display_name":     "Anthropic",
					"last4":            "1234",
					"status":           "active",
					"version":          1,
					"reachable_models": []string{"claude-sonnet-5.5"},
				},
			},
		})
	}))
	defer srv.Close()

	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api03-secretvalue1234")
	t.Setenv("MOONSHOT_API_KEY", "sk-moonshot-testkey-5678")
	t.Setenv(creds.EnvAPIKey, "ork_test_key_1234567890")
	t.Setenv(creds.EnvAPIURL, srv.URL)

	var out, errBuf bytes.Buffer
	opened := ""
	app := &App{
		Prog:    "orchestrai",
		Stdin:   strings.NewReader(""),
		Stdout:  &out,
		Stderr:  &errBuf,
		Dir:     t.TempDir(),
		Store:   &creds.Store{Dir: t.TempDir()},
		HTTP:    srv.Client(),
		OpenURL: func(u string) error { opened = u; return nil },
	}
	if code := app.Run(context.Background(), []string{"keys", "status"}); code != ExitOK {
		t.Fatalf("keys status exited %d: %s", code, errBuf.String())
	}
	text := out.String()
	if strings.Contains(text, "secretvalue") || strings.Contains(text, "testkey") {
		t.Fatalf("output leaked raw secret: %s", text)
	}
	if !strings.Contains(text, "anthropic") || !strings.Contains(text, "[connected]") || !strings.Contains(text, "[not connected]") {
		t.Fatalf("unexpected keys status output:\n%s", text)
	}

	out.Reset()
	if code := app.Run(context.Background(), []string{"keys", "open"}); code != ExitOK {
		t.Fatalf("keys open exited %d: %s", code, errBuf.String())
	}
	if !strings.HasSuffix(opened, "/settings/ai-keys") {
		t.Fatalf("expected browser open to /settings/ai-keys, got %q", opened)
	}
}
