package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// Project is the subset of backend-go ProjectSummary the CLI uses.
type Project struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description *string `json:"description,omitempty"`
	Type        string  `json:"type"`
	Status      string  `json:"status"`
}

// ListProjects pages through GET /api/v1/projects ({projects,total,page,page_size}).
func (c *Client) ListProjects(ctx context.Context) ([]Project, error) {
	var all []Project
	for page := 1; page <= 50; page++ {
		var out struct {
			Projects []Project `json:"projects"`
			Total    int       `json:"total"`
			PageSize int       `json:"page_size"`
		}
		q := url.Values{"page": {strconv.Itoa(page)}, "page_size": {"100"}}
		if err := c.JSON(ctx, Request{Method: http.MethodGet, Path: "/api/v1/projects", Query: q}, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Projects...)
		if len(out.Projects) == 0 || len(all) >= out.Total {
			break
		}
	}
	return all, nil
}

// GetProject fetches GET /api/v1/projects/:id.
func (c *Client) GetProject(ctx context.Context, id string) (*Project, error) {
	var out Project
	if err := c.JSON(ctx, Request{Method: http.MethodGet, Path: "/api/v1/projects/" + PathEscape(id)}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateLiveSpace creates a team space with POST /api/v1/live/spaces {name}
// → 201 {project}. The plan's team-space cap answers 402 (a LimitError).
func (c *Client) CreateLiveSpace(ctx context.Context, name string) (*Project, error) {
	var out struct {
		Project *Project `json:"project"`
	}
	if err := c.JSON(ctx, Request{Method: http.MethodPost, Path: "/api/v1/live/spaces", Body: map[string]string{"name": name}}, &out); err != nil {
		return nil, err
	}
	if out.Project == nil || out.Project.ID == "" {
		return nil, fmt.Errorf("creating the team space returned no id")
	}
	return out.Project, nil
}

// APIKeyInfo is one row of GET /api/v1/api-keys.
type APIKeyInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	KeyPrefix string `json:"key_prefix"`
}

// RevokeAPIKey revokes the key whose 12-char prefix matches apiKey.
func (c *Client) RevokeAPIKey(ctx context.Context, apiKey string) error {
	if len(apiKey) < 12 {
		return fmt.Errorf("API key too short to identify")
	}
	var keys []APIKeyInfo
	if err := c.JSON(ctx, Request{Method: http.MethodGet, Path: "/api/v1/api-keys", Query: url.Values{"limit": {"100"}}}, &keys); err != nil {
		return err
	}
	prefix := apiKey[:12]
	for _, k := range keys {
		if k.KeyPrefix == prefix {
			return c.JSON(ctx, Request{Method: http.MethodDelete, Path: "/api/v1/api-keys/" + PathEscape(k.ID)}, nil)
		}
	}
	return fmt.Errorf("key %s… not found among active API keys", prefix)
}

// FetchBundle GETs the compiled context bundle. etag is sent as
// If-None-Match; ErrNotModified is returned on 304. The raw body is
// returned so callers can decode it with their own types.
func (c *Client) FetchBundle(ctx context.Context, projectID, etag string) (body []byte, newETag string, err error) {
	headers := map[string]string{}
	if etag != "" {
		headers["If-None-Match"] = etag
	}
	resp, err := c.Do(ctx, Request{
		Method:  http.MethodGet,
		Path:    "/api/v1/projects/" + PathEscape(projectID) + "/context/bundle",
		Headers: headers,
	})
	if err != nil {
		return nil, etag, err
	}
	return resp.Body, resp.Header.Get("ETag"), nil
}

// SyncReport is POSTed after a sync (best effort).
type SyncReport struct {
	Client string `json:"client"`
	// ClientVersion lets the server require Repo from current clients.
	ClientVersion string `json:"client_version,omitempty"`
	// Repo identifies the synced repository (normalized remote URL or root
	// directory name); the Free plan caps distinct repos per team space.
	Repo         string       `json:"repo,omitempty"`
	ToolTargets  []string     `json:"tool_targets"`
	ManifestHash string       `json:"manifest_hash"`
	Drift        []DriftEntry `json:"drift"`
}

// DriftEntry describes one managed file whose owned content differs from
// what the manifest recorded, or that was kept because of local edits.
type DriftEntry struct {
	Path  string `json:"path"`
	Tool  string `json:"tool,omitempty"`
	State string `json:"state"`
}

// PostSyncReport sends a sync report.
func (c *Client) PostSyncReport(ctx context.Context, projectID string, r SyncReport) error {
	if r.ToolTargets == nil {
		r.ToolTargets = []string{}
	}
	if r.Drift == nil {
		r.Drift = []DriftEntry{}
	}
	return c.JSON(ctx, Request{
		Method: http.MethodPost,
		Path:   "/api/v1/projects/" + PathEscape(projectID) + "/context/sync-reports",
		Body:   r,
	}, nil)
}

// Workflow is an OrchestrAI automation (workflow_definitions row).
type Workflow struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Version     int       `json:"version"`
	IsActive    bool      `json:"is_active"`
	UpdatedAt   string    `json:"updated_at"`
	Variables   rawObject `json:"variables,omitempty"`
}

type rawObject = map[string]any

// ListWorkflows GETs /projects/:id/workflows/definitions.
func (c *Client) ListWorkflows(ctx context.Context, projectID string) ([]Workflow, error) {
	var out struct {
		Workflows []Workflow `json:"workflows"`
	}
	err := c.JSON(ctx, Request{Method: http.MethodGet, Path: "/api/v1/projects/" + PathEscape(projectID) + "/workflows/definitions"}, &out)
	return out.Workflows, err
}

// NodeExecution is a per-node status inside a workflow execution.
type NodeExecution struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// WorkflowExecution is a workflow_executions row.
type WorkflowExecution struct {
	ID             string                   `json:"id"`
	WorkflowID     string                   `json:"workflow_id"`
	Status         string                   `json:"status"`
	NodeExecutions map[string]NodeExecution `json:"node_executions"`
	OutputPayload  json.RawMessage          `json:"output_payload,omitempty"`
	Error          string                   `json:"error,omitempty"`
}

// ExecuteWorkflow POSTs /projects/:id/workflows/definitions/:wid/execute.
func (c *Client) ExecuteWorkflow(ctx context.Context, projectID, workflowID string, input map[string]any) (*WorkflowExecution, error) {
	if input == nil {
		input = map[string]any{}
	}
	var out WorkflowExecution
	err := c.JSON(ctx, Request{
		Method: http.MethodPost,
		Path:   "/api/v1/projects/" + PathEscape(projectID) + "/workflows/definitions/" + PathEscape(workflowID) + "/execute",
		Body:   map[string]any{"input": input},
	}, &out)
	return &out, err
}

// GetWorkflowExecution GETs one execution.
func (c *Client) GetWorkflowExecution(ctx context.Context, projectID, execID string) (*WorkflowExecution, error) {
	var out WorkflowExecution
	err := c.JSON(ctx, Request{
		Method: http.MethodGet,
		Path:   "/api/v1/projects/" + PathEscape(projectID) + "/workflows/executions/" + PathEscape(execID),
	}, &out)
	return &out, err
}

// Room is a live co-coding room (spec §5 Rooms).
type Room struct {
	ID             string `json:"id"`
	ProjectID      string `json:"project_id"`
	ConversationID string `json:"conversation_id"`
	HostUserID     string `json:"host_user_id"`
	Title          string `json:"title"`
	Status         string `json:"status"`
	PeerCount      int    `json:"peer_count"`
	StartedAt      string `json:"started_at"`
	EndedAt        string `json:"ended_at"`
}

// ListRooms GETs /projects/:id/rooms → {rooms}.
func (c *Client) ListRooms(ctx context.Context, projectID string) ([]Room, error) {
	var out struct {
		Rooms []Room `json:"rooms"`
	}
	err := c.JSON(ctx, Request{Method: http.MethodGet, Path: "/api/v1/projects/" + PathEscape(projectID) + "/rooms"}, &out)
	return out.Rooms, err
}

// GetRoom GETs /rooms/:id (accepts a bare room or {room}).
func (c *Client) GetRoom(ctx context.Context, roomID string) (*Room, error) {
	resp, err := c.Do(ctx, Request{Method: http.MethodGet, Path: "/api/v1/rooms/" + PathEscape(roomID)})
	if err != nil {
		return nil, err
	}
	var wrapped struct {
		Room *Room `json:"room"`
	}
	if json.Unmarshal(resp.Body, &wrapped) == nil && wrapped.Room != nil {
		return wrapped.Room, nil
	}
	var r Room
	if err := json.Unmarshal(resp.Body, &r); err != nil {
		return nil, fmt.Errorf("decode room: %w", err)
	}
	return &r, nil
}

// Conversation is a chat_conversation row.
type Conversation struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// ChatMessage is a chat_message row.
type ChatMessage struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

// CreateConversation POSTs /projects/:id/conversations.
func (c *Client) CreateConversation(ctx context.Context, projectID, title string) (*Conversation, error) {
	var out Conversation
	err := c.JSON(ctx, Request{
		Method: http.MethodPost,
		Path:   "/api/v1/projects/" + PathEscape(projectID) + "/conversations",
		Body:   map[string]string{"title": title},
	}, &out)
	return &out, err
}

// ListMessages GETs a conversation's messages ({messages}).
func (c *Client) ListMessages(ctx context.Context, projectID, conversationID string) ([]ChatMessage, error) {
	var out struct {
		Messages []ChatMessage `json:"messages"`
	}
	err := c.JSON(ctx, Request{
		Method: http.MethodGet,
		Path:   "/api/v1/projects/" + PathEscape(projectID) + "/conversations/" + PathEscape(conversationID) + "/messages",
		Query:  url.Values{"limit": {"500"}},
	}, &out)
	return out.Messages, err
}

// AddMessage persists a transcript message.
func (c *Client) AddMessage(ctx context.Context, projectID, conversationID, role, content string) (*ChatMessage, error) {
	var out ChatMessage
	err := c.JSON(ctx, Request{
		Method: http.MethodPost,
		Path:   "/api/v1/projects/" + PathEscape(projectID) + "/conversations/" + PathEscape(conversationID) + "/messages",
		Body:   map[string]string{"role": role, "content": content},
	}, &out)
	return &out, err
}

// SendChat POSTs /projects/:id/chat/message (202 Accepted; the reply streams over /ws).
func (c *Client) SendChat(ctx context.Context, projectID, conversationID, message string, history []map[string]string) error {
	body := map[string]any{"message": message, "conversation_id": conversationID}
	if len(history) > 0 {
		body["conversation_history"] = history
	}
	return c.JSON(ctx, Request{
		Method: http.MethodPost,
		Path:   "/api/v1/projects/" + PathEscape(projectID) + "/chat/message",
		Body:   body,
	}, nil)
}

// WSTicket mints a short-lived WebSocket ticket (POST /api/v1/ws/ticket).
func (c *Client) WSTicket(ctx context.Context) (string, error) {
	var out struct {
		Ticket string `json:"ticket"`
	}
	if err := c.JSON(ctx, Request{Method: http.MethodPost, Path: "/api/v1/ws/ticket"}, &out); err != nil {
		return "", err
	}
	if out.Ticket == "" {
		return "", fmt.Errorf("ws ticket response had no ticket")
	}
	return out.Ticket, nil
}

// AuditCostFinding is one cloud cost savings opportunity from /live/audit-summary.
type AuditCostFinding struct {
	Resource          string  `json:"resource"`
	Provider          string  `json:"provider,omitempty"`
	Category          string  `json:"category,omitempty"`
	MonthlySavingsUSD float64 `json:"monthly_savings_usd"`
	Description       string  `json:"description"`
	Recommendation    string  `json:"recommendation,omitempty"`
}

// AuditSecurityFinding is one failed cloud security control from /live/audit-summary.
type AuditSecurityFinding struct {
	ID          string `json:"id"`
	Severity    string `json:"severity"`
	Title       string `json:"title"`
	Resource    string `json:"resource,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Framework   string `json:"framework,omitempty"`
	Remediation string `json:"remediation,omitempty"`
}

// AuditSummary is the GET /api/v1/projects/:projectId/live/audit-summary response.
type AuditSummary struct {
	Connected              bool                   `json:"connected"`
	Providers              []string               `json:"providers,omitempty"`
	MonthlySavingsUSD      float64                `json:"monthly_savings_usd"`
	CostFindings           []AuditCostFinding     `json:"cost_findings,omitempty"`
	FailedSecurityControls int                    `json:"failed_security_controls"`
	SecurityFindings       []AuditSecurityFinding `json:"security_findings,omitempty"`
	ScannedAt              string                 `json:"scanned_at,omitempty"`
	ConnectURL             string                 `json:"connect_url,omitempty"`
	BillingURL             string                 `json:"billing_url,omitempty"`
	Raw                    json.RawMessage        `json:"-"`
}

// GetAuditSummary GETs /api/v1/projects/:projectId/live/audit-summary.
// Decodes backend-go's LiveAuditSummary wire shape ({has_cloud_connection,
// cost: {available, findings_count, monthly_savings_usd, top_findings},
// security: {available, pass, fail, failed_controls}, upgrade_url}) as well as
// flat summary shapes.
func (c *Client) GetAuditSummary(ctx context.Context, projectID string) (*AuditSummary, error) {
	resp, err := c.Do(ctx, Request{
		Method: http.MethodGet,
		Path:   "/api/v1/projects/" + PathEscape(projectID) + "/live/audit-summary",
	})
	if err != nil {
		return nil, err
	}
	data := resp.Body
	var wrapped struct {
		Summary json.RawMessage `json:"summary"`
	}
	if json.Unmarshal(data, &wrapped) == nil && len(wrapped.Summary) > 0 && string(wrapped.Summary) != "null" {
		data = wrapped.Summary
	}
	var out AuditSummary
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode audit summary: %w", err)
	}
	// Decode backend-go's nested LiveAuditSummary shape and alternative field names.
	var nested struct {
		HasCloudConnection bool   `json:"has_cloud_connection"`
		UpgradeURL         string `json:"upgrade_url"`
		Cost               *struct {
			Available         bool    `json:"available"`
			FindingsCount     int     `json:"findings_count"`
			MonthlySavingsUSD float64 `json:"monthly_savings_usd"`
			TopFindings       []struct {
				Kind              string  `json:"kind"`
				ResourceID        string  `json:"resource_id"`
				Title             string  `json:"title"`
				MonthlySavingsUSD float64 `json:"monthly_savings_usd"`
				Provider          string  `json:"provider"`
				Region            string  `json:"region"`
			} `json:"top_findings"`
		} `json:"cost"`
		Security *struct {
			Available      bool `json:"available"`
			Pass           int  `json:"pass"`
			Fail           int  `json:"fail"`
			FailedControls []struct {
				ControlID string `json:"control_id"`
				Title     string `json:"title"`
				Severity  string `json:"severity"`
				Resource  string `json:"resource"`
			} `json:"failed_controls"`
		} `json:"security"`
		ConnectedProviders     []string `json:"connected_providers"`
		TotalMonthlySavingsUSD float64  `json:"total_monthly_savings_usd"`
		FailedControlsCount    int      `json:"failed_controls_count"`
	}
	if json.Unmarshal(data, &nested) == nil {
		if nested.HasCloudConnection {
			out.Connected = true
		}
		if out.BillingURL == "" && nested.UpgradeURL != "" {
			out.BillingURL = nested.UpgradeURL
		}
		if nested.Cost != nil {
			if nested.Cost.Available && !out.Connected {
				out.Connected = true
			}
			if out.MonthlySavingsUSD == 0 && nested.Cost.MonthlySavingsUSD > 0 {
				out.MonthlySavingsUSD = nested.Cost.MonthlySavingsUSD
			}
			if len(out.CostFindings) == 0 && len(nested.Cost.TopFindings) > 0 {
				seenProv := map[string]bool{}
				for _, p := range out.Providers {
					seenProv[p] = true
				}
				for _, f := range nested.Cost.TopFindings {
					res := f.ResourceID
					if res == "" {
						res = f.Kind
					}
					if res == "" {
						res = "cloud-resource"
					}
					out.CostFindings = append(out.CostFindings, AuditCostFinding{
						Resource:          res,
						Provider:          f.Provider,
						Category:          f.Kind,
						MonthlySavingsUSD: f.MonthlySavingsUSD,
						Description:       f.Title,
					})
					if f.Provider != "" && !seenProv[f.Provider] {
						seenProv[f.Provider] = true
						out.Providers = append(out.Providers, f.Provider)
					}
				}
			}
		}
		if nested.Security != nil {
			if nested.Security.Available && !out.Connected {
				out.Connected = true
			}
			if out.FailedSecurityControls == 0 && nested.Security.Fail > 0 {
				out.FailedSecurityControls = nested.Security.Fail
			}
			if len(out.SecurityFindings) == 0 && len(nested.Security.FailedControls) > 0 {
				for _, f := range nested.Security.FailedControls {
					out.SecurityFindings = append(out.SecurityFindings, AuditSecurityFinding{
						ID:       f.ControlID,
						Severity: f.Severity,
						Title:    f.Title,
						Resource: f.Resource,
					})
				}
			}
		}
		if len(out.Providers) == 0 && len(nested.ConnectedProviders) > 0 {
			out.Providers = nested.ConnectedProviders
		}
		if out.MonthlySavingsUSD == 0 && nested.TotalMonthlySavingsUSD > 0 {
			out.MonthlySavingsUSD = nested.TotalMonthlySavingsUSD
		}
		if out.FailedSecurityControls == 0 && nested.FailedControlsCount > 0 {
			out.FailedSecurityControls = nested.FailedControlsCount
		}
	}
	if !out.Connected && len(out.Providers) > 0 {
		out.Connected = true
	}
	if out.FailedSecurityControls == 0 && len(out.SecurityFindings) > 0 {
		out.FailedSecurityControls = len(out.SecurityFindings)
	}
	if out.MonthlySavingsUSD == 0 && len(out.CostFindings) > 0 {
		for _, f := range out.CostFindings {
			out.MonthlySavingsUSD += f.MonthlySavingsUSD
		}
	}
	out.Raw = append(json.RawMessage(nil), data...)
	return &out, nil
}
