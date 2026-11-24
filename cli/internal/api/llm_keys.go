package api

import (
	"context"
	"net/http"
)

// LLMProviderKey is the public summary returned by GET /api/v1/llm-keys.
type LLMProviderKey struct {
	ID              string   `json:"id"`
	OwnerType       string   `json:"owner_type"`
	Provider        string   `json:"provider"`
	DisplayName     string   `json:"display_name"`
	Last4           string   `json:"last4"`
	Status          string   `json:"status"`
	Version         int      `json:"version"`
	ReachableModels []string `json:"reachable_models"`
}

// LLMKeyPolicy is the org key policy returned by GET /api/v1/llm-keys.
type LLMKeyPolicy struct {
	PreferOrgKeys        bool   `json:"prefer_org_keys"`
	DisallowPersonalKeys bool   `json:"disallow_personal_keys"`
	ZDROnly              bool   `json:"zdr_only"`
	OrganizationID       string `json:"organization_id,omitempty"`
}

// LLMKeysResponse is the GET /api/v1/llm-keys payload.
type LLMKeysResponse struct {
	Keys   []LLMProviderKey `json:"keys"`
	Policy *LLMKeyPolicy    `json:"policy,omitempty"`
}

// ListLLMKeys fetches GET /api/v1/llm-keys (never returns secrets).
func (c *Client) ListLLMKeys(ctx context.Context) (*LLMKeysResponse, error) {
	var out LLMKeysResponse
	if err := c.JSON(ctx, Request{Method: http.MethodGet, Path: "/api/v1/llm-keys"}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
