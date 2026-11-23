package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// wsMessage is backend-go's WebSocketMessage envelope.
type wsMessage struct {
	Type  string          `json:"type"`
	RunID string          `json:"run_id"`
	Data  json.RawMessage `json:"data"`
}

type tokenData struct {
	RunID  string `json:"run_id"`
	Delta  string `json:"delta"`
	Tokens struct {
		Delta        string `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"tokens"`
	FinishReason string `json:"finish_reason"`
}

type statusData struct {
	RunID          string         `json:"run_id"`
	Status         string         `json:"status"`
	Decision       string         `json:"decision"`
	Message        string         `json:"message"`
	Error          string         `json:"error"`
	ConversationID string         `json:"conversation_id"`
	Interaction    map[string]any `json:"interaction"`
	Metadata       map[string]any `json:"metadata"`
}

// wsURLFor derives ws(s)://host/ws from the API URL.
func wsURLFor(apiURL string) (string, error) {
	if v := os.Getenv(EnvWSURL); v != "" {
		return strings.TrimRight(v, "/"), nil
	}
	u, err := url.Parse(apiURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws"
	return u.String(), nil
}

func (a *App) cmdAsk(ctx context.Context, args []string) error {
	fs := a.flags("ask", `"<question>" [--conversation <id>] [flags]`)
	conv := fs.String("conversation", "", "continue this conversation (default: start a new one)")
	timeout := fs.Duration("timeout", 10*time.Minute, "give up waiting for the reply after this long")
	projectFlag := fs.String("project", "", "team space id")
	apiFlag := fs.String("api-url", "", "API base URL")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	question := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if question == "-" {
		data, err := io.ReadAll(io.LimitReader(a.Stdin, 1<<20))
		if err != nil {
			return err
		}
		question = strings.TrimSpace(string(data))
	}
	if question == "" {
		return usagef(`ask needs a question, e.g. %s ask "why is the deploy failing?"`, a.Prog)
	}
	s, err := a.projectSession(*apiFlag, *projectFlag)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	// Connect the stream before sending so no token is missed.
	ticket, err := s.client.WSTicket(ctx)
	if err != nil {
		return err
	}
	base, err := wsURLFor(s.apiURL)
	if err != nil {
		return err
	}
	q := url.Values{"project_id": {s.projectID}, "ticket": {ticket}}
	dialOpts := &websocket.DialOptions{}
	if a.HTTP != nil {
		// The stream is long-lived: keep the transport, drop the timeout.
		hc := *a.HTTP
		hc.Timeout = 0
		dialOpts.HTTPClient = &hc
	}
	conn, _, err := websocket.Dial(ctx, base+"?"+q.Encode(), dialOpts)
	if err != nil {
		return fmt.Errorf("connect to the live stream: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(4 << 20)

	conversationID := *conv
	var history []map[string]string
	if conversationID == "" {
		title := question
		if r := []rune(title); len(r) > 60 {
			title = string(r[:59]) + "…"
		}
		c, err := s.client.CreateConversation(ctx, s.projectID, title)
		if err != nil {
			return err
		}
		conversationID = c.ID
	} else {
		msgs, err := s.client.ListMessages(ctx, s.projectID, conversationID)
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if (m.Role == "user" || m.Role == "assistant") && m.Content != "" {
				history = append(history, map[string]string{"role": m.Role, "content": m.Content})
			}
		}
		if len(history) > 20 {
			history = history[len(history)-20:]
		}
	}
	if err := s.client.SendChat(ctx, s.projectID, conversationID, question, history); err != nil {
		return err
	}
	if _, err := s.client.AddMessage(ctx, s.projectID, conversationID, "user", question); err != nil {
		fmt.Fprintf(a.Stderr, "Warning: could not save your message to the conversation (%v)\n", err)
	}

	reply, outcome, err := a.streamReply(ctx, conn, conversationID)
	if reply != "" {
		if !strings.HasSuffix(reply, "\n") {
			fmt.Fprintln(a.Stdout)
		}
		// Persist the assistant turn like the web chat does (best effort).
		saveCtx, saveCancel := context.WithTimeout(context.Background(), 15*time.Second)
		if _, serr := s.client.AddMessage(saveCtx, s.projectID, conversationID, "assistant", reply); serr != nil {
			fmt.Fprintf(a.Stderr, "Warning: could not save the reply to the conversation (%v)\n", serr)
		}
		saveCancel()
	}
	fmt.Fprintf(a.Stderr, "\nConversation %s — continue with `%s ask --conversation %s \"…\"`\n", conversationID, a.Prog, conversationID)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return &exitError{code: ExitAPI, err: fmt.Errorf("no complete reply within %s; the agent may still be working — see %s", *timeout, s.appURL)}
		}
		return err
	}
	switch outcome {
	case "failed":
		return silentExit(ExitRemoteRun)
	case "approval":
		fmt.Fprintf(a.Stderr, "The agent is waiting for approval. Review it at %s.\n", s.appURL)
	}
	return nil
}

// streamReply prints tokens for the conversation (and runs it spawns)
// until the reply finishes. outcome is "done", "clarification",
// "approval" or "failed".
func (a *App) streamReply(ctx context.Context, conn *websocket.Conn, conversationID string) (string, string, error) {
	known := map[string]bool{conversationID: true}
	var reply strings.Builder
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return reply.String(), "", ctx.Err()
			}
			return reply.String(), "", fmt.Errorf("live stream closed: %w", err)
		}
		var msg wsMessage
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.Type {
		case "tokens":
			var td tokenData
			if json.Unmarshal(msg.Data, &td) != nil {
				continue
			}
			runID := td.RunID
			if runID == "" {
				runID = msg.RunID
			}
			if !known[runID] {
				continue
			}
			delta := td.Tokens.Delta
			if delta == "" {
				delta = td.Delta
			}
			if delta != "" {
				reply.WriteString(delta)
				fmt.Fprint(a.Stdout, delta)
			}
			fin := td.Tokens.FinishReason
			if fin == "" {
				fin = td.FinishReason
			}
			if fin != "" && fin != "tool_calls" && runID == conversationID {
				return reply.String(), "done", nil
			}
		case "agent_status", "agent_decision":
			var sd statusData
			if json.Unmarshal(msg.Data, &sd) != nil {
				continue
			}
			runID := sd.RunID
			if runID == "" {
				runID = msg.RunID
			}
			convOf := sd.ConversationID
			if convOf == "" && sd.Metadata != nil {
				convOf, _ = sd.Metadata["conversation_id"].(string)
			}
			if !known[runID] {
				if convOf != conversationID {
					continue
				}
				known[runID] = true
				fmt.Fprintf(a.Stderr, "[agent started run %s]\n", runID)
			}
			status := strings.ToLower(sd.Status)
			switch status {
			case "clarification":
				a.printClarification(sd)
				return reply.String(), "clarification", nil
			case "pending_confirmation", "awaiting_approval", "approval_required", "waiting_approval":
				return reply.String(), "approval", nil
			case "failed", "error", "cancelled", "canceled":
				msgText := sd.Error
				if msgText == "" {
					msgText = sd.Message
				}
				if msgText != "" {
					fmt.Fprintf(a.Stderr, "\nThe agent run %s: %s\n", status, msgText)
				}
				return reply.String(), "failed", nil
			case "completed", "succeeded":
				if runID != conversationID || reply.Len() > 0 {
					return reply.String(), "done", nil
				}
			}
		case "approval_required":
			var sd statusData
			if json.Unmarshal(msg.Data, &sd) == nil && known[sd.RunID] {
				return reply.String(), "approval", nil
			}
		case "error":
			var e struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(msg.Data, &e) == nil && e.Message != "" {
				return reply.String(), "", fmt.Errorf("live stream error: %s", e.Message)
			}
		}
	}
}

func (a *App) printClarification(sd statusData) {
	inter := sd.Interaction
	if inter == nil && sd.Metadata != nil {
		inter, _ = sd.Metadata["interaction"].(map[string]any)
	}
	question := sd.Message
	for _, k := range []string{"question", "prompt", "message", "title"} {
		if v, ok := inter[k].(string); ok && v != "" {
			question = v
			break
		}
	}
	if question != "" {
		fmt.Fprintf(a.Stdout, "\nThe agent needs more information: %s\n", question)
	}
	for _, k := range []string{"options", "choices"} {
		opts, ok := inter[k].([]any)
		if !ok {
			continue
		}
		for i, o := range opts {
			label := fmt.Sprint(o)
			if m, ok := o.(map[string]any); ok {
				for _, lk := range []string{"label", "title", "value"} {
					if v, ok := m[lk].(string); ok && v != "" {
						label = v
						break
					}
				}
			}
			fmt.Fprintf(a.Stdout, "  %d) %s\n", i+1, label)
		}
	}
}
