package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/OrchestrAIio/orchestrai/cli/internal/api"
)

func (a *App) cmdWorkflows(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(a.Stderr, "Usage: %s workflows <list|run> [flags]\n", a.Prog)
		if len(args) == 0 {
			return usagef("missing workflows subcommand")
		}
		return nil
	}
	switch args[0] {
	case "list", "ls":
		return a.workflowsList(ctx, args[1:])
	case "run":
		return a.workflowsRun(ctx, args[1:])
	}
	return usagef("unknown workflows subcommand %q (list, run)", args[0])
}

func (a *App) workflowsList(ctx context.Context, args []string) error {
	fs := a.flags("workflows list", "[flags]")
	asJSON := fs.Bool("json", false, "print JSON")
	projectFlag := fs.String("project", "", "team space id")
	apiFlag := fs.String("api-url", "", "API base URL")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	s, err := a.projectSession(*apiFlag, *projectFlag)
	if err != nil {
		return err
	}
	wfs, err := s.client.ListWorkflows(ctx, s.projectID)
	if err != nil {
		return err
	}
	if *asJSON {
		if wfs == nil {
			wfs = []api.Workflow{}
		}
		return a.printJSON(map[string]any{"workflows": wfs})
	}
	if len(wfs) == 0 {
		fmt.Fprintf(a.Stdout, "No automations in this team space yet. Create one under Automations at %s.\n", s.appURL)
		return nil
	}
	tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tID\tACTIVE\tVERSION\tDESCRIPTION")
	for _, w := range wfs {
		fmt.Fprintf(tw, "%s\t%s\t%t\t%d\t%s\n", w.Name, w.ID, w.IsActive, w.Version, truncate(w.Description, 50))
	}
	return tw.Flush()
}

// parseInputs turns k=v (string) and k:=<json> (typed) pairs into a map.
func parseInputs(pairs []string, jsonBlob string) (map[string]any, error) {
	in := map[string]any{}
	if jsonBlob != "" {
		dec := json.NewDecoder(strings.NewReader(jsonBlob))
		dec.UseNumber()
		if err := dec.Decode(&in); err != nil {
			return nil, usagef("--input-json is not a JSON object: %v", err)
		}
	}
	for _, p := range pairs {
		if k, v, ok := strings.Cut(p, ":="); ok && !strings.Contains(k, "=") {
			if k == "" {
				return nil, usagef("invalid --input %q", p)
			}
			var val any
			dec := json.NewDecoder(strings.NewReader(v))
			dec.UseNumber()
			if err := dec.Decode(&val); err != nil {
				return nil, usagef("--input %s:= value is not valid JSON: %v", k, err)
			}
			in[k] = val
			continue
		}
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, usagef("invalid --input %q (use key=value or key:=<json>)", p)
		}
		in[k] = v
	}
	return in, nil
}

func terminalStatus(s string) bool {
	switch strings.ToLower(s) {
	case "completed", "succeeded", "success", "failed", "error", "cancelled", "canceled", "expired", "timed_out", "rejected":
		return true
	}
	return false
}

func failedStatus(s string) bool {
	switch strings.ToLower(s) {
	case "completed", "succeeded", "success":
		return false
	}
	return true
}

func (a *App) workflowsRun(ctx context.Context, args []string) error {
	fs := a.flags("workflows run", "<name|id> [--input key=value]... [flags]")
	var inputs stringList
	fs.Var(&inputs, "input", "workflow input as key=value (string) or key:=<json>; repeatable")
	inputJSON := fs.String("input-json", "", "workflow input as a JSON object")
	wait := fs.Bool("wait", false, "wait for the run to finish and print node progress")
	timeout := fs.Duration("timeout", 30*time.Minute, "with --wait: give up after this long")
	asJSON := fs.Bool("json", false, "print JSON")
	projectFlag := fs.String("project", "", "team space id")
	apiFlag := fs.String("api-url", "", "API base URL")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("workflows run needs the workflow name or id")
	}
	input, err := parseInputs(inputs, *inputJSON)
	if err != nil {
		return err
	}
	s, err := a.projectSession(*apiFlag, *projectFlag)
	if err != nil {
		return err
	}
	wfs, err := s.client.ListWorkflows(ctx, s.projectID)
	if err != nil {
		return err
	}
	want := fs.Arg(0)
	var match []api.Workflow
	for _, w := range wfs {
		if w.ID == want {
			match = []api.Workflow{w}
			break
		}
		if strings.EqualFold(w.Name, want) {
			match = append(match, w)
		}
	}
	switch len(match) {
	case 0:
		return fmt.Errorf("no automation named %q in this team space (see `%s workflows list`)", want, a.Prog)
	case 1:
	default:
		ids := make([]string, len(match))
		for i, m := range match {
			ids[i] = m.ID
		}
		return usagef("%q matches %d automations; run one by id: %s", want, len(match), strings.Join(ids, ", "))
	}
	wf := match[0]
	exec, err := s.client.ExecuteWorkflow(ctx, s.projectID, wf.ID, input)
	if err != nil {
		return err
	}
	if !*wait {
		if *asJSON {
			return a.printJSON(exec)
		}
		fmt.Fprintf(a.Stdout, "Started %s: execution %s (%s).\n", wf.Name, exec.ID, orDash(exec.Status))
		return nil
	}
	if !*asJSON {
		fmt.Fprintf(a.Stderr, "Started %s: execution %s. Waiting…\n", wf.Name, exec.ID)
	}
	deadline := a.Now().Add(*timeout)
	seen := map[string]string{}
	for !terminalStatus(exec.Status) {
		if !a.Now().Before(deadline) {
			return &exitError{code: ExitAPI, err: fmt.Errorf("timed out after %s; execution %s is still %s", *timeout, exec.ID, exec.Status)}
		}
		if err := a.Sleep(ctx, 2*time.Second); err != nil {
			return err
		}
		next, err := s.client.GetWorkflowExecution(ctx, s.projectID, exec.ID)
		if err != nil {
			return err
		}
		exec = next
		if !*asJSON {
			nodes := make([]string, 0, len(exec.NodeExecutions))
			for id := range exec.NodeExecutions {
				nodes = append(nodes, id)
			}
			sort.Strings(nodes)
			for _, id := range nodes {
				st := exec.NodeExecutions[id].Status
				if seen[id] != st {
					seen[id] = st
					fmt.Fprintf(a.Stderr, "  %-10s %s\n", st, id)
				}
			}
		}
	}
	if *asJSON {
		if err := a.printJSON(exec); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(a.Stdout, "Execution %s %s.\n", exec.ID, exec.Status)
		if exec.Error != "" {
			fmt.Fprintf(a.Stdout, "  Error: %s\n", exec.Error)
		}
		if len(exec.OutputPayload) > 0 && string(exec.OutputPayload) != "null" {
			fmt.Fprintf(a.Stdout, "  Output: %s\n", exec.OutputPayload)
		}
	}
	if failedStatus(exec.Status) {
		return silentExit(ExitRemoteRun)
	}
	return nil
}
