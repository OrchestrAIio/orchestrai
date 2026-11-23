package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestParseErrorEnvelopes(t *testing.T) {
	tests := []struct {
		status   int
		body     string
		wantMsg  string
		wantCode string
	}{
		{404, `{"error":"Project not found"}`, "Project not found", ""},
		{401, `{"error":{"code":"UNAUTHORIZED","message":"Invalid API key"}}`, "Invalid API key", "UNAUTHORIZED"},
		{500, `<html>boom</html>`, "<html>boom</html>", ""},
		{400, `{"error":"bad","code":"INVALID_ATTACHMENT"}`, "bad", "INVALID_ATTACHMENT"},
	}
	for _, tc := range tests {
		err := ParseError(tc.status, []byte(tc.body))
		var ae *APIError
		if !errors.As(err, &ae) || ae.Message != tc.wantMsg || ae.Code != tc.wantCode || ae.Status != tc.status {
			t.Errorf("%s → %#v", tc.body, err)
		}
	}
	err := ParseError(402, []byte(`{"error":"Free plan allows 25 rules","code":"LIVE_LIMIT_HUB_RULES","cta":"upgrade","limit":25,"current":25,"upgrade_url":"https://app.test/settings/billing?from=live_hub","buy_credits_url":"https://app.test/credits"}`))
	var le *LimitError
	if !errors.As(err, &le) {
		t.Fatalf("402 not a LimitError: %v", err)
	}
	if le.Code != "LIVE_LIMIT_HUB_RULES" || le.Limit.String() != "25" || le.CTA != "upgrade" || le.ActionURL() != "https://app.test/settings/billing?from=live_hub" {
		t.Fatalf("limit error = %+v", le)
	}
	le.CTA = "buy_credits"
	if le.ActionURL() != "https://app.test/credits" {
		t.Fatal("buy_credits CTA should prefer buy_credits_url")
	}
}

// fakeClock records sleeps without waiting.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func (f *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sleeps = append(f.sleeps, d)
	f.now = f.now.Add(d)
	return nil
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func deviceServer(t *testing.T, script []func(w http.ResponseWriter)) (*httptest.Server, *int) {
	t.Helper()
	polls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/editor/connect/start", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["client"] != "ork" || body["device_name"] == "" {
			http.Error(w, `{"error":"bad"}`, 400)
			return
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("start must be unauthenticated")
		}
		_ = json.NewEncoder(w).Encode(DeviceStart{DeviceCode: "dev-1", UserCode: "ABCD-EFGH", VerificationURI: "https://app.test/live/connect", VerificationURIComplete: "https://app.test/live/connect?code=ABCD-EFGH", Interval: 2, ExpiresIn: 30})
	})
	mux.HandleFunc("POST /api/v1/editor/connect/poll", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["device_code"] != "dev-1" {
			t.Errorf("device_code = %q", body["device_code"])
		}
		i := polls
		polls++
		if i >= len(script) {
			w.WriteHeader(428)
			_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
			return
		}
		script[i](w)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &polls
}

func pending(w http.ResponseWriter) {
	w.WriteHeader(428)
	_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
}

func TestDeviceFlowPolling(t *testing.T) {
	ok := func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"api_key":"ork_abcdefghijklmnopqrstuvwxyz","user":{"id":"u1","name":"Ada","email":"ada@test"}}`))
	}
	slow := func(w http.ResponseWriter) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"slow_down"}`))
	}
	flaky := func(w http.ResponseWriter) { w.WriteHeader(503) }
	srv, polls := deviceServer(t, []func(http.ResponseWriter){pending, slow, flaky, pending, ok})
	c := New(srv.URL, "", "test")
	start, err := c.StartDeviceFlow(context.Background(), "ork", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: time.Unix(0, 0)}
	res, err := c.WaitForAuthorization(context.Background(), start, clock.Sleep, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	if res.APIKey == "" || res.User.Email != "ada@test" || *polls != 5 {
		t.Fatalf("res=%+v polls=%d", res, *polls)
	}
	want := []time.Duration{2 * time.Second, 2 * time.Second, 7 * time.Second, 7 * time.Second, 7 * time.Second}
	if len(clock.sleeps) != len(want) {
		t.Fatalf("sleeps = %v", clock.sleeps)
	}
	for i := range want {
		if clock.sleeps[i] != want[i] {
			t.Fatalf("sleeps = %v, want %v (interval honoured, +5s after slow_down)", clock.sleeps, want)
		}
	}
}

func TestDeviceFlowTerminalStates(t *testing.T) {
	expired := func(w http.ResponseWriter) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"expired_token"}`))
	}
	denied := func(w http.ResponseWriter) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":"access_denied"}`))
	}
	for name, tc := range map[string]struct {
		script []func(http.ResponseWriter)
		want   error
	}{
		"expired": {[]func(http.ResponseWriter){pending, expired}, ErrExpiredToken},
		"denied":  {[]func(http.ResponseWriter){denied}, ErrAccessDenied},
		"timeout": {nil, ErrExpiredToken}, // pending forever until expires_in elapses
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := deviceServer(t, tc.script)
			c := New(srv.URL, "", "test")
			start, err := c.StartDeviceFlow(context.Background(), "ork", "laptop")
			if err != nil {
				t.Fatal(err)
			}
			clock := &fakeClock{now: time.Unix(0, 0)}
			_, err = c.WaitForAuthorization(context.Background(), start, clock.Sleep, clock.Now)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
}

func TestFetchBundleETag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ork_key" {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("If-None-Match") == `"v2"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v2"`)
		_, _ = w.Write([]byte(`{"version":"2","hash":"h","files":[]}`))
	}))
	defer srv.Close()
	c := New(srv.URL+"/", "ork_key", "test")
	body, etag, err := c.FetchBundle(context.Background(), "p1", "")
	if err != nil || etag != `"v2"` || len(body) == 0 {
		t.Fatalf("first fetch: %v %q", err, etag)
	}
	if _, _, err := c.FetchBundle(context.Background(), "p1", etag); !errors.Is(err, ErrNotModified) {
		t.Fatalf("expected 304, got %v", err)
	}
}

func TestParseRef(t *testing.T) {
	tests := []struct{ in, pub, slug, id, ver string }{
		{"acme/review", "acme", "review", "", ""},
		{"@acme/review@1.2.0", "acme", "review", "", "1.2.0"},
		{"3f2a-uuid", "", "", "3f2a-uuid", ""},
		{"3f2a-uuid@2", "", "", "3f2a-uuid", "2"},
	}
	for _, tc := range tests {
		pub, slug, id, ver, err := ParseRef(tc.in)
		if err != nil || pub != tc.pub || slug != tc.slug || id != tc.id || ver != tc.ver {
			t.Errorf("%s → %q %q %q %q %v", tc.in, pub, slug, id, ver, err)
		}
	}
	for _, bad := range []string{"", "a/b/c", "/x", "x@"} {
		if _, _, _, _, err := ParseRef(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestClientSendsClientHeaderAndSyncRepo(t *testing.T) {
	var gotClient, gotAuth string
	var report map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotClient, gotAuth = r.Header.Get(ClientHeader), r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&report)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "ork_test", "orchestrai-cli/test")
	if err := c.PostSyncReport(context.Background(), "p1", SyncReport{Client: "ork", Repo: "github.com/acme/api"}); err != nil {
		t.Fatal(err)
	}
	if gotClient != "ork" || gotAuth != "Bearer ork_test" {
		t.Fatalf("headers: client=%q auth=%q", gotClient, gotAuth)
	}
	if report["repo"] != "github.com/acme/api" || report["client"] != "ork" {
		t.Fatalf("report = %v", report)
	}
}
