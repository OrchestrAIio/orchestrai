// Package api is the HTTP client for the OrchestrAI backend (backend-go).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultAPIURL is the production backend used when nothing else is configured.
const DefaultAPIURL = "https://orchestrai-api-production.up.railway.app"

// DefaultAppURL is the production web app (device approval, billing, rooms).
const DefaultAppURL = "https://app.orchestrai.io"

// maxResponseBytes bounds every response body the CLI will buffer.
const maxResponseBytes = 64 << 20

// Client talks to /api/v1 with an optional ork_ API key.
type Client struct {
	BaseURL   string
	APIKey    string
	UserAgent string
	// ClientName is sent as X-OrchestrAI-Client so the backend knows which
	// surface a turn came from (non-web clients can't approve runs by chat).
	ClientName string
	HTTP       *http.Client
}

// ClientHeader names the calling client to the backend.
const ClientHeader = "X-OrchestrAI-Client"

// DefaultClientName is this CLI's client id.
const DefaultClientName = "ork"

// New returns a client for baseURL (trailing slashes removed).
func New(baseURL, apiKey, userAgent string) *Client {
	return &Client{
		BaseURL:    NormalizeURL(baseURL),
		APIKey:     apiKey,
		UserAgent:  userAgent,
		ClientName: DefaultClientName,
		HTTP:       &http.Client{Timeout: 60 * time.Second},
	}
}

// NormalizeURL trims whitespace and trailing slashes.
func NormalizeURL(u string) string {
	return strings.TrimRight(strings.TrimSpace(u), "/")
}

// ValidateBaseURL checks that u is an absolute http(s) URL.
func ValidateBaseURL(u string) error {
	parsed, err := url.Parse(u)
	if err != nil {
		return fmt.Errorf("invalid API URL %q: %w", u, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("invalid API URL %q: scheme must be http or https", u)
	}
	if parsed.Host == "" {
		return fmt.Errorf("invalid API URL %q: missing host", u)
	}
	return nil
}

// APIError is any non-2xx response that is not a plan-limit denial.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.Code != "" {
		return fmt.Sprintf("%s (HTTP %d, %s)", msg, e.Status, e.Code)
	}
	return fmt.Sprintf("%s (HTTP %d)", msg, e.Status)
}

// LimitError is an HTTP 402 denial (spec §4): a Live limit or a required
// purchase. CTA is "buy_credits" or "upgrade".
type LimitError struct {
	Message       string
	Code          string
	CTA           string
	Limit         json.Number
	Current       json.Number
	UpgradeURL    string
	BuyCreditsURL string
}

func (e *LimitError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "plan limit reached"
}

// ActionURL is the link matching the CTA (buy credits first when that is
// what the server asks for).
func (e *LimitError) ActionURL() string {
	if e.CTA == "buy_credits" && e.BuyCreditsURL != "" {
		return e.BuyCreditsURL
	}
	if e.UpgradeURL != "" {
		return e.UpgradeURL
	}
	return e.BuyCreditsURL
}

// ErrNotModified is returned by conditional GETs that hit 304.
var ErrNotModified = errors.New("not modified")

// IsStatus reports whether err is an APIError with the given status.
func IsStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

// errorBody covers the three error envelopes backend-go emits:
// {"error":"msg"}, {"error":{"code","message"}} and the 402 limit body.
type errorBody struct {
	Error         json.RawMessage `json:"error"`
	Message       string          `json:"message"`
	Code          string          `json:"code"`
	CTA           string          `json:"cta"`
	Limit         json.Number     `json:"limit"`
	Current       json.Number     `json:"current"`
	UpgradeURL    string          `json:"upgrade_url"`
	BuyCreditsURL string          `json:"buy_credits_url"`
}

// ParseError converts a non-2xx response body into an error value.
func ParseError(status int, body []byte) error {
	var eb errorBody
	msg, code := "", ""
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&eb); err == nil {
		code = eb.Code
		msg = eb.Message
		if len(eb.Error) > 0 {
			var s string
			if json.Unmarshal(eb.Error, &s) == nil {
				msg = s
			} else {
				var detail struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				}
				if json.Unmarshal(eb.Error, &detail) == nil {
					if detail.Message != "" {
						msg = detail.Message
					}
					if code == "" {
						code = detail.Code
					}
				}
			}
		}
	} else {
		msg = strings.TrimSpace(string(body))
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
	}
	if status == http.StatusPaymentRequired {
		return &LimitError{
			Message: msg, Code: code, CTA: eb.CTA, Limit: eb.Limit, Current: eb.Current,
			UpgradeURL: eb.UpgradeURL, BuyCreditsURL: eb.BuyCreditsURL,
		}
	}
	return &APIError{Status: status, Code: code, Message: msg}
}

// Request is one API call.
type Request struct {
	Method  string
	Path    string // starting with /api/v1/...
	Query   url.Values
	Body    any
	Headers map[string]string
	NoAuth  bool
}

// Response carries the raw result of a call.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Do performs req. Non-2xx statuses (other than 304) become errors.
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	u := c.BaseURL + req.Path
	if len(req.Query) > 0 {
		u += "?" + req.Query.Encode()
	}
	var body io.Reader
	if req.Body != nil {
		buf, err := json.Marshal(req.Body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(buf)
	}
	hreq, err := http.NewRequestWithContext(ctx, req.Method, u, body)
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Accept", "application/json")
	if req.Body != nil {
		hreq.Header.Set("Content-Type", "application/json")
	}
	if c.UserAgent != "" {
		hreq.Header.Set("User-Agent", c.UserAgent)
	}
	if c.ClientName != "" {
		hreq.Header.Set(ClientHeader, c.ClientName)
	}
	if !req.NoAuth && c.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	for k, v := range req.Headers {
		hreq.Header.Set(k, v)
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(hreq)
	if err != nil {
		return nil, &NetworkError{Err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, &NetworkError{Err: err}
	}
	out := &Response{Status: resp.StatusCode, Header: resp.Header, Body: data}
	if resp.StatusCode == http.StatusNotModified {
		return out, ErrNotModified
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return out, ParseError(resp.StatusCode, data)
	}
	return out, nil
}

// NetworkError wraps transport failures.
type NetworkError struct{ Err error }

func (e *NetworkError) Error() string { return "network error: " + e.Err.Error() }
func (e *NetworkError) Unwrap() error { return e.Err }

// JSON performs req and decodes a 2xx body into out (if out is non-nil).
func (c *Client) JSON(ctx context.Context, req Request, out any) error {
	resp, err := c.Do(ctx, req)
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(resp.Body)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(resp.Body))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decode %s %s response: %w", req.Method, req.Path, err)
	}
	return nil
}

// PathEscape escapes one path segment.
func PathEscape(s string) string { return url.PathEscape(s) }
