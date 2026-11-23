package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"unicode"

	"github.com/OrchestrAIio/orchestrai/cli/internal/api"
	"github.com/OrchestrAIio/orchestrai/cli/internal/creds"
)

func (a *App) cmdLogin(ctx context.Context, args []string) error {
	fs := a.flags("login", "[flags]")
	apiFlag := fs.String("api-url", "", "API base URL")
	noBrowser := fs.Bool("no-browser", false, "print the sign-in link instead of opening a browser")
	deviceName := fs.String("device-name", "", "name shown on the API key (default: hostname)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("login takes no arguments")
	}
	rc, err := a.repo()
	if err != nil {
		return err
	}
	u, err := a.apiURL(*apiFlag, rc.Cfg)
	if err != nil {
		return err
	}
	if err := a.deviceLogin(ctx, u, *deviceName, *noBrowser); err != nil {
		return err
	}
	if rc.Bound {
		fmt.Fprintf(a.Stdout, "Next: %s sync\n", a.Prog)
	} else {
		fmt.Fprintf(a.Stdout, "Next: %s init\n", a.Prog)
	}
	return nil
}

// deviceLogin runs the browser device flow against apiURL and stores the key.
func (a *App) deviceLogin(ctx context.Context, u, deviceName string, noBrowser bool) error {
	name := deviceName
	if name == "" {
		h, _ := os.Hostname()
		name = h
	}
	c := a.newClient(u, "")
	start, err := c.StartDeviceFlow(ctx, DeviceClient, stableDeviceName(name))
	if err != nil {
		return err
	}
	link := start.VerificationURIComplete
	if link == "" {
		link = start.VerificationURI
	}
	fmt.Fprintf(a.Stderr, "To sign in, open this link and confirm the code %s:\n\n  %s\n\n", start.UserCode, link)
	if !noBrowser && a.OpenURL != nil {
		if err := a.OpenURL(link); err == nil {
			fmt.Fprintln(a.Stderr, "Opened your browser. Waiting for approval…")
		} else {
			fmt.Fprintln(a.Stderr, "Waiting for approval…")
		}
	} else {
		fmt.Fprintln(a.Stderr, "Waiting for approval…")
	}
	res, err := c.WaitForAuthorization(ctx, start, a.Sleep, a.Now)
	if err != nil {
		return err
	}
	where, err := a.Store.Save(u, res.APIKey, creds.User{ID: res.User.ID, Name: res.User.Name, Email: res.User.Email})
	if err != nil {
		return fmt.Errorf("signed in, but saving credentials failed: %w", err)
	}
	loc := "the OS keychain"
	if where == "file" {
		loc = a.Store.Path()
	}
	fmt.Fprintf(a.Stdout, "Signed in as %s to %s (API key stored in %s).\n", userLabel(res.User.Name, res.User.Email), u, loc)
	if os.Getenv(creds.EnvAPIKey) != "" {
		fmt.Fprintf(a.Stderr, "Note: %s is set and takes precedence over the stored key.\n", creds.EnvAPIKey)
	}
	return nil
}

// stableDeviceName is the device_name sent with the device-flow start: the
// host name cleaned the way the server stores it (no control characters,
// trimmed, at most 64 characters), or "this device". The editor extension
// and the ACP bridge send the same value from the same machine; the API has
// no separate device identifier field.
func stableDeviceName(host string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(host) {
		if !unicode.IsControl(r) {
			b.WriteRune(r)
		}
	}
	name := strings.TrimSpace(b.String())
	if r := []rune(name); len(r) > 64 {
		name = strings.TrimSpace(string(r[:64]))
	}
	if name == "" {
		return "this device"
	}
	return name
}

func userLabel(name, email string) string {
	switch {
	case name != "" && email != "":
		return fmt.Sprintf("%s <%s>", name, email)
	case email != "":
		return email
	case name != "":
		return name
	}
	return "your account"
}

func (a *App) cmdLogout(ctx context.Context, args []string) error {
	fs := a.flags("logout", "[flags]")
	apiFlag := fs.String("api-url", "", "API base URL")
	keepKey := fs.Bool("keep-key", false, "only forget the key locally; do not revoke it on the server")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	rc, err := a.repo()
	if err != nil {
		return err
	}
	u, err := a.apiURL(*apiFlag, rc.Cfg)
	if err != nil {
		return err
	}
	if os.Getenv(creds.EnvAPIKey) != "" {
		fmt.Fprintf(a.Stderr, "Note: %s is set in the environment; unset it to stop using that key.\n", creds.EnvAPIKey)
	}
	f, err := a.Store.Load()
	if err != nil {
		return err
	}
	if _, ok := f.Accounts[u]; !ok {
		fmt.Fprintf(a.Stdout, "Not signed in to %s.\n", u)
		return nil
	}
	if !*keepKey {
		if cr, gerr := a.Store.GetStored(u); gerr == nil {
			if err := a.newClient(u, cr.APIKey).RevokeAPIKey(ctx, cr.APIKey); err != nil {
				fmt.Fprintf(a.Stderr, "Warning: could not revoke the API key on the server (%v); revoke it in Settings → API keys.\n", err)
			}
		}
	}
	if _, err := a.Store.Delete(u); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "Signed out of %s.\n", u)
	return nil
}

func (a *App) cmdWhoami(ctx context.Context, args []string) error {
	fs := a.flags("whoami", "[flags]")
	apiFlag := fs.String("api-url", "", "API base URL")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	rc, err := a.repo()
	if err != nil {
		return err
	}
	u, err := a.apiURL(*apiFlag, rc.Cfg)
	if err != nil {
		return err
	}
	c, cr, err := a.authedClient(u)
	if err != nil {
		return err
	}
	// Validate the key with a cheap authenticated call.
	verr := c.JSON(ctx, api.Request{Method: http.MethodGet, Path: "/api/v1/projects", Query: url.Values{"page_size": {"1"}}}, nil)
	if verr != nil && (api.IsStatus(verr, http.StatusUnauthorized) || api.IsStatus(verr, http.StatusForbidden)) {
		return verr
	}
	out := map[string]any{
		"api_url": u, "source": cr.Source, "user": cr.User, "valid": verr == nil,
	}
	if rc.Cfg != nil {
		out["project_id"] = rc.Cfg.ProjectID
	}
	if *asJSON {
		return a.printJSON(out)
	}
	who := userLabel(cr.User.Name, cr.User.Email)
	if cr.Source == "env" {
		who = "API key from " + creds.EnvAPIKey
	}
	fmt.Fprintf(a.Stdout, "%s\n  API:     %s\n  Key in:  %s\n", who, u, sourceLabel(cr.Source, a.Store.Path()))
	if rc.Cfg != nil {
		fmt.Fprintf(a.Stdout, "  Project: %s (%s)\n", rc.Cfg.ProjectID, rc.Root)
	}
	if verr != nil {
		fmt.Fprintf(a.Stdout, "  Warning: could not verify the key: %v\n", verr)
	}
	return nil
}

func sourceLabel(src, path string) string {
	switch src {
	case "env":
		return "environment"
	case "keyring":
		return "OS keychain"
	}
	return path
}
