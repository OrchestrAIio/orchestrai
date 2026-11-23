package cli

import (
	"context"
	"fmt"
	"strings"
)

func (a *App) cmdAudit(ctx context.Context, args []string) error {
	fs := a.flags("audit", "[--project <id>] [--json] [flags]")
	asJSON := fs.Bool("json", false, "print JSON")
	projectFlag := fs.String("project", "", "team space id")
	apiFlag := fs.String("api-url", "", "API base URL")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("audit takes no positional arguments")
	}
	s, err := a.projectSession(*apiFlag, *projectFlag)
	if err != nil {
		return err
	}
	summary, err := s.client.GetAuditSummary(ctx, s.projectID)
	if err != nil {
		return err
	}
	if *asJSON {
		return a.printJSON(summary)
	}

	w := a.Stdout
	appBase := strings.TrimRight(s.appURL, "/")
	if !summary.Connected {
		connectURL := summary.ConnectURL
		if connectURL == "" {
			connectURL = fmt.Sprintf("%s/projects/%s/settings/cloud", appBase, s.projectID)
		}
		fmt.Fprintln(w, "No cloud accounts are connected to this team space yet.")
		fmt.Fprintln(w, "Connect AWS, GCP, or Azure with read-only permissions in ~60 seconds to see instant cost savings and failed security controls:")
		fmt.Fprintf(w, "  %s\n", connectURL)
		return nil
	}

	providers := "connected cloud"
	if len(summary.Providers) > 0 {
		providers = strings.Join(summary.Providers, ", ")
	}
	fmt.Fprintf(w, "Cloud Audit Summary (%s)\n", providers)
	fmt.Fprintf(w, "  Estimated monthly cost savings: $%.2f/mo\n", summary.MonthlySavingsUSD)
	fmt.Fprintf(w, "  Failed security controls:       %d\n", summary.FailedSecurityControls)

	if len(summary.CostFindings) > 0 {
		fmt.Fprintln(w, "\nTop cost savings opportunities:")
		for _, c := range summary.CostFindings {
			fmt.Fprintf(w, "  -$%.2f/mo  %s — %s\n", c.MonthlySavingsUSD, c.Resource, c.Description)
		}
	}

	if len(summary.SecurityFindings) > 0 {
		fmt.Fprintln(w, "\nFailed security controls:")
		for _, f := range summary.SecurityFindings {
			sev := strings.ToUpper(orDash(f.Severity))
			res := ""
			if f.Resource != "" {
				res = " (" + f.Resource + ")"
			}
			fmt.Fprintf(w, "  [%s] %s%s\n", sev, f.Title, res)
		}
	}

	billingURL := summary.BillingURL
	if billingURL == "" {
		billingURL = appBase + "/settings/billing?from=live_cli_audit"
	} else if strings.HasPrefix(billingURL, "/") {
		billingURL = appBase + billingURL
	}
	fmt.Fprintf(w, "\nFix with OrchestrAI Agent (from $5 = 100 credits, or Pro $29/mo): %s\n", billingURL)
	fmt.Fprintf(w, "  Run: %s ask \"Remediate the cloud audit findings and open a PR\"\n", a.Prog)
	return nil
}
