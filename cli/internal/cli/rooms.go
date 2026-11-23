package cli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"text/tabwriter"
)

// ExtensionID is the editor extension that handles room deep links.
const ExtensionID = "orchestrai.orchestrai-live"

func (a *App) cmdRooms(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(a.Stderr, "Usage: %s rooms <list|open> [flags]\n", a.Prog)
		if len(args) == 0 {
			return usagef("missing rooms subcommand")
		}
		return nil
	}
	switch args[0] {
	case "list", "ls":
		return a.roomsList(ctx, args[1:])
	case "open":
		return a.roomsOpen(ctx, args[1:])
	}
	return usagef("unknown rooms subcommand %q (list, open)", args[0])
}

func (a *App) roomsList(ctx context.Context, args []string) error {
	fs := a.flags("rooms list", "[flags]")
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
	rooms, err := s.client.ListRooms(ctx, s.projectID)
	if err != nil {
		return err
	}
	if *asJSON {
		return a.printJSON(map[string]any{"rooms": rooms})
	}
	if len(rooms) == 0 {
		fmt.Fprintln(a.Stdout, "No rooms. Start one from the editor extension or the web app.")
		return nil
	}
	tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTITLE\tSTATUS\tPEERS\tSTARTED")
	for _, r := range rooms {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", r.ID, orDash(r.Title), r.Status, r.PeerCount, orDash(r.StartedAt))
	}
	return tw.Flush()
}

// defaultEditor guesses the editor the terminal runs in.
func defaultEditor() string {
	switch {
	case os.Getenv("CURSOR_TRACE_ID") != "":
		return "cursor"
	case os.Getenv("WINDSURF_TRACE_ID") != "" || os.Getenv("TERM_PROGRAM") == "windsurf":
		return "windsurf"
	}
	return "vscode"
}

// roomURL builds the deep link (editor) or web URL (browser) for a room.
func roomURL(editor, appURL, projectID, roomID string) (string, error) {
	switch editor {
	case "browser", "web":
		return fmt.Sprintf("%s/projects/%s/rooms/%s", appURL, url.PathEscape(projectID), url.PathEscape(roomID)), nil
	case "vscode", "vscode-insiders", "cursor", "windsurf", "vscodium":
		q := url.Values{"room": {roomID}, "project": {projectID}}
		return fmt.Sprintf("%s://%s/room?%s", editor, ExtensionID, q.Encode()), nil
	}
	return "", usagef("invalid --editor %q (vscode, vscode-insiders, cursor, windsurf, vscodium, browser)", editor)
}

func (a *App) roomsOpen(ctx context.Context, args []string) error {
	fs := a.flags("rooms open", "<room-id> [--editor vscode|cursor|windsurf|vscodium|browser] [flags]")
	editor := fs.String("editor", "", "where to open the room (default: detected editor, else vscode)")
	printOnly := fs.Bool("print", false, "print the link instead of opening it")
	projectFlag := fs.String("project", "", "team space id")
	apiFlag := fs.String("api-url", "", "API base URL")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("rooms open needs a room id (see `%s rooms list`)", a.Prog)
	}
	s, err := a.projectSession(*apiFlag, *projectFlag)
	if err != nil {
		return err
	}
	room, err := s.client.GetRoom(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	if room.Status == "ended" {
		return fmt.Errorf("room %s has ended", room.ID)
	}
	pid := room.ProjectID
	if pid == "" {
		pid = s.projectID
	}
	ed := *editor
	if ed == "" {
		ed = defaultEditor()
	}
	link, err := roomURL(ed, s.appURL, pid, room.ID)
	if err != nil {
		return err
	}
	if *printOnly || a.OpenURL == nil {
		fmt.Fprintln(a.Stdout, link)
		return nil
	}
	if err := a.OpenURL(link); err != nil {
		fmt.Fprintf(a.Stdout, "Open this link to join: %s\n", link)
		return nil
	}
	fmt.Fprintf(a.Stdout, "Opening room %q in %s…\n", orDash(room.Title), ed)
	return nil
}
