package cli

import (
	"context"
	"fmt"
)

func (a *App) cmdInit(ctx context.Context, args []string) error {
	fs := a.flags("init", "[--project <id> | --create-space <name>] [flags]")
	projectFlag := fs.String("project", "", "team space (project) id to bind")
	createFlag := fs.String("create-space", "", "create a team space with this name and bind it")
	yes := fs.Bool("yes", false, "never prompt: use the only team space, or create one named after the repository when you have none")
	apiFlag := fs.String("api-url", "", "API base URL")
	toolsFlag := fs.String("tools", "", "default tool targets for sync, e.g. claude,cursor (default: all)")
	force := fs.Bool("force", false, "rebind a repository that is already bound to another team space")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("init takes no arguments (use --project <id> or --create-space <name>)")
	}
	if *projectFlag != "" && *createFlag != "" {
		return usagef("pass either --project or --create-space, not both")
	}
	tools, err := parseTools(*toolsFlag)
	if err != nil {
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
	c, err := a.signedInClient(ctx, u, true)
	if err != nil {
		return err
	}
	chosen, err := a.chooseSpace(ctx, c, rc, onboardOpts{projectID: *projectFlag, createSpace: *createFlag, yes: *yes})
	if err != nil {
		return err
	}
	if err := a.bindRepo(rc, chosen, u, tools, *force); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "Next: %s sync\n", a.Prog)
	return nil
}
