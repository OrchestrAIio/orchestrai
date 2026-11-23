package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/OrchestrAIio/orchestrai/cli/internal/api"
	"github.com/OrchestrAIio/orchestrai/cli/internal/creds"
	"github.com/OrchestrAIio/orchestrai/cli/internal/library"
	"github.com/OrchestrAIio/orchestrai/cli/internal/project"
	"github.com/OrchestrAIio/orchestrai/cli/internal/syncer"
)

// Exit codes (documented in README and `orchestrai help`).
const (
	ExitOK          = 0
	ExitOutOfSync   = 1 // sync --check / status --exit-code / diff --exit-code found changes
	ExitUsage       = 2
	ExitAuth        = 3 // not signed in, invalid or revoked key, forbidden
	ExitLimit       = 4 // HTTP 402: plan limit, upgrade or credits required
	ExitAPI         = 5 // network failure or unexpected API response
	ExitRefused     = 6 // unsafe path, malformed local file, or secrets in a publish
	ExitNotBound    = 7 // repository not bound (no .orchestrai/config.json)
	ExitFailure     = 8 // local I/O or other error
	ExitRemoteRun   = 9 // a workflow run or agent request finished in a failed state
	ExitInterrupted = 130
)

// usageError is a bad invocation.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error { return &usageError{msg: fmt.Sprintf(format, args...)} }

// exitError carries an explicit exit code; quiet means the message was
// already printed.
type exitError struct {
	code  int
	err   error
	quiet bool
}

func (e *exitError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit %d", e.code)
	}
	return e.err.Error()
}

func (e *exitError) Unwrap() error { return e.err }

func silentExit(code int) error { return &exitError{code: code, quiet: true} }

// exitCode maps an error to the process exit code.
func exitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	var ue *usageError
	var le *api.LimitError
	var ae *api.APIError
	var ne *api.NetworkError
	var pe *syncer.PathError
	var ce *syncer.ContentError
	var se *library.SecretsError
	switch {
	case errors.As(err, &ue):
		return ExitUsage
	case errors.Is(err, context.Canceled):
		return ExitInterrupted
	case errors.As(err, &le):
		return ExitLimit
	case errors.Is(err, creds.ErrNotLoggedIn):
		return ExitAuth
	case errors.As(err, &ae):
		if ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden {
			return ExitAuth
		}
		return ExitAPI
	case errors.As(err, &ne), errors.Is(err, context.DeadlineExceeded):
		return ExitAPI
	case errors.As(err, &pe), errors.As(err, &ce), errors.As(err, &se):
		return ExitRefused
	case errors.Is(err, project.ErrNotInitialized):
		return ExitNotBound
	}
	return ExitFailure
}

// printError renders err for humans on w.
func printError(w io.Writer, prog string, err error, appURL string) {
	var ee *exitError
	if errors.As(err, &ee) && ee.quiet {
		return
	}
	var le *api.LimitError
	if errors.As(err, &le) {
		printLimit(w, le, appURL)
		return
	}
	var ue *usageError
	if errors.As(err, &ue) {
		fmt.Fprintf(w, "%s: %s\nRun `%s help` for usage.\n", prog, withProg(ue.msg, prog), prog)
		return
	}
	msg := withProg(err.Error(), prog)
	var ae *api.APIError
	if errors.As(err, &ae) && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden) {
		if ae.Status == http.StatusUnauthorized {
			msg += "\nYour sign-in is missing, expired or revoked. Run `" + prog + " login`."
		} else {
			msg += "\nYour account or API key does not have access to this resource."
		}
	}
	fmt.Fprintf(w, "%s: %s\n", prog, msg)
}

// withProg rewrites the `orchestrai …` hints in messages from other packages
// (creds, project) to the name the CLI was run as, e.g. `npx @orchestrai/cli …`.
func withProg(msg, prog string) string {
	if prog == "" || prog == "orchestrai" {
		return msg
	}
	return strings.ReplaceAll(msg, "`orchestrai ", "`"+prog+" ")
}

// printLimit renders a 402 the same way every OrchestrAI client does: the
// server's message plus the matching call to action.
func printLimit(w io.Writer, le *api.LimitError, appURL string) {
	fmt.Fprintf(w, "%s\n", le.Error())
	if le.Limit.String() != "" || le.Current.String() != "" {
		fmt.Fprintf(w, "  Usage: %s of %s\n", limitValue(le.Code, le.Current), limitValue(le.Code, le.Limit))
	}
	cta := le.CTA
	if cta == "" && strings.Contains(strings.ToUpper(le.Code), "CREDIT") {
		cta = "buy_credits"
	}
	billing := strings.TrimRight(appURL, "/") + "/settings/billing?from=live_cli"
	switch cta {
	case "buy_credits":
		u := le.BuyCreditsURL
		if u == "" {
			u = billing
		}
		fmt.Fprintf(w, "  Buy credits to continue: %s\n", u)
		if le.UpgradeURL != "" {
			fmt.Fprintf(w, "  Or upgrade your plan:     %s\n", le.UpgradeURL)
		}
	default:
		u := le.UpgradeURL
		if u == "" {
			u = billing
		}
		fmt.Fprintf(w, "  Upgrade your plan: %s\n", u)
		if le.BuyCreditsURL != "" {
			fmt.Fprintf(w, "  Or buy credits:    %s\n", le.BuyCreditsURL)
		}
	}
}

// limitValue formats a 402 limit or usage by the unit in its LIVE_LIMIT_<NAME>
// code: hours or minutes for *_SECONDS*, GiB/MiB/KB for *_BYTES*, days for
// *_DAYS, a plain count otherwise; "-" when absent, "unlimited" for -1.
func limitValue(code string, n json.Number) string {
	raw := n.String()
	if raw == "" || raw == "-1" {
		return orDash(raw)
	}
	v, err := n.Float64()
	if err != nil || v < 0 {
		return raw
	}
	name := strings.ToUpper(code)
	trim := func(x float64) string { return strconv.FormatFloat(math.Round(x*10)/10, 'f', -1, 64) }
	unit := func(x float64, one, many string) string {
		if x == 1 {
			return trim(x) + " " + one
		}
		return trim(x) + " " + many
	}
	switch {
	case strings.Contains(name, "_SECONDS"):
		if v >= 3600 {
			return unit(v/3600, "hour", "hours")
		}
		return unit(math.Ceil(v/60), "minute", "minutes")
	case strings.Contains(name, "_BYTES"):
		switch {
		case v >= 1<<30:
			return trim(v/(1<<30)) + " GiB"
		case v >= 1<<20:
			return trim(v/(1<<20)) + " MiB"
		}
		return strconv.FormatFloat(math.Ceil(v/1024), 'f', -1, 64) + " KB"
	case strings.HasSuffix(name, "_DAYS"):
		return unit(v, "day", "days")
	}
	return raw
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	if s == "-1" {
		return "unlimited"
	}
	return s
}
