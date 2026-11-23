package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// LibraryItem is a library catalog entry (spec §6). Unknown fields are kept
// in Raw so `library show --json` can print everything the server sent.
type LibraryItem struct {
	ID            string          `json:"id"`
	Publisher     string          `json:"publisher"`
	Slug          string          `json:"slug"`
	Name          string          `json:"name"`
	Kind          string          `json:"kind"`
	Scope         string          `json:"scope"`
	Description   string          `json:"description"`
	Status        string          `json:"status"`
	LatestVersion string          `json:"latest_version"`
	Version       string          `json:"version"`
	ToolCompat    []string        `json:"tool_compat"`
	InstallCount  int             `json:"install_count"`
	URL           string          `json:"url,omitempty"`
	Body          json.RawMessage `json:"body,omitempty"`
	Files         []LibraryFile   `json:"files,omitempty"`
	Raw           json.RawMessage `json:"-"`
}

// Ref is the human reference: publisher/slug when known, else the id.
func (i LibraryItem) Ref() string {
	if i.Publisher != "" && i.Slug != "" {
		return i.Publisher + "/" + i.Slug
	}
	return i.ID
}

// DisplayVersion prefers latest_version.
func (i LibraryItem) DisplayVersion() string {
	if i.LatestVersion != "" {
		return i.LatestVersion
	}
	return i.Version
}

// LibraryFile is one file of an item version.
type LibraryFile struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Encoding string `json:"encoding,omitempty"` // "" (utf-8) or "base64"
	SHA256   string `json:"sha256,omitempty"`
}

func decodeItem(data []byte) (*LibraryItem, error) {
	var wrapped struct {
		Item json.RawMessage `json:"item"`
	}
	if json.Unmarshal(data, &wrapped) == nil && len(wrapped.Item) > 0 && string(wrapped.Item) != "null" {
		data = wrapped.Item
	}
	var it LibraryItem
	if err := json.Unmarshal(data, &it); err != nil {
		return nil, fmt.Errorf("decode library item: %w", err)
	}
	it.Raw = append(json.RawMessage(nil), data...)
	return &it, nil
}

func decodeItems(data []byte) ([]LibraryItem, error) {
	var raws []json.RawMessage
	var wrapped struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(data, &wrapped); err == nil && wrapped.Items != nil {
		raws = wrapped.Items
	} else if err := json.Unmarshal(data, &raws); err != nil {
		return nil, fmt.Errorf("decode library items: %w", err)
	}
	out := make([]LibraryItem, 0, len(raws))
	for _, r := range raws {
		it, err := decodeItem(r)
		if err != nil {
			return nil, err
		}
		out = append(out, *it)
	}
	return out, nil
}

// ListLibraryItems GETs /api/v1/library/items?scope=&kind=&q=.
func (c *Client) ListLibraryItems(ctx context.Context, scope, kind, q string) ([]LibraryItem, error) {
	query := url.Values{}
	if scope != "" {
		query.Set("scope", scope)
	}
	if kind != "" {
		query.Set("kind", kind)
	}
	if q != "" {
		query.Set("q", q)
	}
	resp, err := c.Do(ctx, Request{Method: http.MethodGet, Path: "/api/v1/library/items", Query: query})
	if err != nil {
		return nil, err
	}
	return decodeItems(resp.Body)
}

// GetLibraryItem GETs /api/v1/library/items/:id[/versions/:v].
func (c *Client) GetLibraryItem(ctx context.Context, id, version string) (*LibraryItem, error) {
	p := "/api/v1/library/items/" + PathEscape(id)
	if version != "" {
		p += "/versions/" + PathEscape(version)
	}
	resp, err := c.Do(ctx, Request{Method: http.MethodGet, Path: p})
	if err != nil {
		return nil, err
	}
	return decodeItem(resp.Body)
}

// GetPublicLibraryItem resolves publisher/slug through the public, unauthenticated
// gallery API: GET /api/v1/public/library/items/:publisher/:slug[/versions/:v].
func (c *Client) GetPublicLibraryItem(ctx context.Context, publisher, slug, version string) (*LibraryItem, error) {
	p := "/api/v1/public/library/items/" + PathEscape(publisher) + "/" + PathEscape(slug)
	if version != "" {
		p += "/versions/" + PathEscape(version)
	}
	resp, err := c.Do(ctx, Request{Method: http.MethodGet, Path: p, NoAuth: c.APIKey == ""})
	if err != nil {
		return nil, err
	}
	return decodeItem(resp.Body)
}

// PublishRequest creates a library item (POST /api/v1/library/items) or,
// with ItemID set, a new version (POST /api/v1/library/items/:id/versions).
type PublishRequest struct {
	Scope       string         `json:"scope"`
	Kind        string         `json:"kind"`
	Name        string         `json:"name"`
	Slug        string         `json:"slug"`
	Description string         `json:"description,omitempty"`
	Version     string         `json:"version,omitempty"`
	URL         string         `json:"url,omitempty"`
	ToolCompat  []string       `json:"tool_compat,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	Files       []LibraryFile  `json:"files"`
}

// ErrItemExists is returned when the create call reports a slug conflict
// without telling us which item to version.
var ErrItemExists = errors.New("a library item with this slug already exists in that scope; pass --item <id> to publish a new version")

// PublishLibraryItem creates the item; on a 409 that names the existing
// item_id it publishes a new version of that item instead.
func (c *Client) PublishLibraryItem(ctx context.Context, itemID string, req PublishRequest) (*LibraryItem, bool, error) {
	if req.Files == nil {
		req.Files = []LibraryFile{}
	}
	if itemID != "" {
		it, err := c.publishVersion(ctx, itemID, req)
		return it, false, err
	}
	resp, err := c.Do(ctx, Request{Method: http.MethodPost, Path: "/api/v1/library/items", Body: req})
	if err == nil {
		it, derr := decodeItem(resp.Body)
		return it, true, derr
	}
	if resp != nil && resp.Status == http.StatusConflict {
		var conflict struct {
			ItemID string `json:"item_id"`
		}
		if json.Unmarshal(resp.Body, &conflict) == nil && conflict.ItemID != "" {
			it, verr := c.publishVersion(ctx, conflict.ItemID, req)
			return it, false, verr
		}
		return nil, false, ErrItemExists
	}
	return nil, false, err
}

func (c *Client) publishVersion(ctx context.Context, itemID string, req PublishRequest) (*LibraryItem, error) {
	resp, err := c.Do(ctx, Request{Method: http.MethodPost, Path: "/api/v1/library/items/" + PathEscape(itemID) + "/versions", Body: req})
	if err != nil {
		return nil, err
	}
	it, err := decodeItem(resp.Body)
	if err != nil {
		return nil, err
	}
	if it.ID == "" {
		it.ID = itemID
	}
	return it, nil
}

// Install is a library install into a project.
type Install struct {
	ID        string       `json:"id"`
	ItemID    string       `json:"item_id"`
	Version   string       `json:"version"`
	Item      *LibraryItem `json:"item,omitempty"`
	Publisher string       `json:"publisher"`
	Slug      string       `json:"slug"`
	Name      string       `json:"name"`
	Kind      string       `json:"kind"`
}

// Ref returns publisher/slug when known.
func (in Install) Ref() string {
	switch {
	case in.Item != nil && in.Item.Publisher != "" && in.Item.Slug != "":
		return in.Item.Publisher + "/" + in.Item.Slug
	case in.Publisher != "" && in.Slug != "":
		return in.Publisher + "/" + in.Slug
	}
	return in.ItemID
}

func decodeInstall(data []byte) (*Install, error) {
	var wrapped struct {
		Install *Install `json:"install"`
	}
	if json.Unmarshal(data, &wrapped) == nil && wrapped.Install != nil {
		return wrapped.Install, nil
	}
	var in Install
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, fmt.Errorf("decode install: %w", err)
	}
	return &in, nil
}

// CreateInstall POSTs /projects/:id/library/installs {item_id, version}.
func (c *Client) CreateInstall(ctx context.Context, projectID, itemID, version string) (*Install, error) {
	body := map[string]string{"item_id": itemID}
	if version != "" {
		body["version"] = version
	}
	resp, err := c.Do(ctx, Request{Method: http.MethodPost, Path: "/api/v1/projects/" + PathEscape(projectID) + "/library/installs", Body: body})
	if err != nil {
		return nil, err
	}
	return decodeInstall(resp.Body)
}

// ListInstalls GETs /projects/:id/library/installs ({installs} or array).
func (c *Client) ListInstalls(ctx context.Context, projectID string) ([]Install, error) {
	resp, err := c.Do(ctx, Request{Method: http.MethodGet, Path: "/api/v1/projects/" + PathEscape(projectID) + "/library/installs"})
	if err != nil {
		return nil, err
	}
	var wrapped struct {
		Installs []Install `json:"installs"`
	}
	if err := json.Unmarshal(resp.Body, &wrapped); err == nil && wrapped.Installs != nil {
		return wrapped.Installs, nil
	}
	var arr []Install
	if err := json.Unmarshal(resp.Body, &arr); err != nil {
		return nil, fmt.Errorf("decode installs: %w", err)
	}
	return arr, nil
}

// DeleteInstall DELETEs /projects/:id/library/installs/:installId.
func (c *Client) DeleteInstall(ctx context.Context, projectID, installID string) error {
	return c.JSON(ctx, Request{Method: http.MethodDelete, Path: "/api/v1/projects/" + PathEscape(projectID) + "/library/installs/" + PathEscape(installID)}, nil)
}

// ParseRef splits "publisher/slug@version" or "id@version".
func ParseRef(ref string) (publisher, slug, id, version string, err error) {
	ref = strings.TrimSpace(ref)
	if at := strings.LastIndex(ref, "@"); at > 0 {
		version = ref[at+1:]
		ref = ref[:at]
		if version == "" {
			return "", "", "", "", fmt.Errorf("invalid reference %q: empty version after @", ref)
		}
	}
	if ref == "" {
		return "", "", "", "", fmt.Errorf("empty library reference")
	}
	if strings.Contains(ref, "/") {
		parts := strings.Split(ref, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", "", "", "", fmt.Errorf("invalid reference %q: expected <publisher>/<slug>", ref)
		}
		return strings.TrimPrefix(parts[0], "@"), parts[1], "", version, nil
	}
	return "", "", ref, version, nil
}
