package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// DeviceStart is the response of POST /api/v1/editor/connect/start.
type DeviceStart struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	Interval                int    `json:"interval"`
	ExpiresIn               int    `json:"expires_in"`
}

// User is the signed-in account.
type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// DeviceResult is the successful poll response.
type DeviceResult struct {
	APIKey string `json:"api_key"`
	User   User   `json:"user"`
}

// Device-flow terminal and transient states (RFC 8628 §3.5).
var (
	ErrAuthorizationPending = errors.New("authorization_pending")
	ErrSlowDown             = errors.New("slow_down")
	ErrExpiredToken         = errors.New("the sign-in code expired; run login again")
	ErrAccessDenied         = errors.New("sign-in was denied in the browser")
)

// StartDeviceFlow begins editor sign-in for the given client and device name.
func (c *Client) StartDeviceFlow(ctx context.Context, client, deviceName string) (*DeviceStart, error) {
	var out DeviceStart
	err := c.JSON(ctx, Request{
		Method: http.MethodPost,
		Path:   "/api/v1/editor/connect/start",
		Body:   map[string]string{"client": client, "device_name": deviceName},
		NoAuth: true,
	}, &out)
	if err != nil {
		return nil, err
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		return nil, fmt.Errorf("sign-in start response is missing device_code/user_code")
	}
	return &out, nil
}

// PollDeviceFlow makes one poll. Pending/slow-down/expired/denied map to the
// sentinel errors above.
func (c *Client) PollDeviceFlow(ctx context.Context, deviceCode string) (*DeviceResult, error) {
	resp, err := c.Do(ctx, Request{
		Method: http.MethodPost,
		Path:   "/api/v1/editor/connect/poll",
		Body:   map[string]string{"device_code": deviceCode},
		NoAuth: true,
	})
	if err != nil {
		if resp != nil {
			var body struct {
				Error json.RawMessage `json:"error"`
			}
			var code string
			if json.Unmarshal(resp.Body, &body) == nil && len(body.Error) > 0 {
				_ = json.Unmarshal(body.Error, &code)
			}
			switch code {
			case "authorization_pending":
				return nil, ErrAuthorizationPending
			case "slow_down":
				return nil, ErrSlowDown
			case "expired_token":
				return nil, ErrExpiredToken
			case "access_denied":
				return nil, ErrAccessDenied
			}
			if resp.Status == http.StatusPreconditionRequired {
				return nil, ErrAuthorizationPending
			}
			if resp.Status == http.StatusTooManyRequests {
				return nil, ErrSlowDown
			}
		}
		return nil, err
	}
	var out DeviceResult
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("decode sign-in result: %w", err)
	}
	if out.APIKey == "" {
		return nil, fmt.Errorf("sign-in result did not include an API key")
	}
	return &out, nil
}

// Sleeper waits for d or until ctx is done.
type Sleeper func(ctx context.Context, d time.Duration) error

// RealSleep is the production Sleeper.
func RealSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// WaitForAuthorization polls until the user approves, honouring interval,
// slow_down (+5 s per RFC 8628) and expires_in. Transient network errors
// are retried until the code expires.
func (c *Client) WaitForAuthorization(ctx context.Context, start *DeviceStart, sleep Sleeper, now func() time.Time) (*DeviceResult, error) {
	if sleep == nil {
		sleep = RealSleep
	}
	if now == nil {
		now = time.Now
	}
	interval := time.Duration(start.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	expiresIn := time.Duration(start.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = 15 * time.Minute
	}
	deadline := now().Add(expiresIn)
	for {
		if err := sleep(ctx, interval); err != nil {
			return nil, err
		}
		if !now().Before(deadline) {
			return nil, ErrExpiredToken
		}
		res, err := c.PollDeviceFlow(ctx, start.DeviceCode)
		switch {
		case err == nil:
			return res, nil
		case errors.Is(err, ErrAuthorizationPending):
			continue
		case errors.Is(err, ErrSlowDown):
			interval += 5 * time.Second
			continue
		case errors.Is(err, ErrExpiredToken), errors.Is(err, ErrAccessDenied):
			return nil, err
		}
		var ne *NetworkError
		if errors.As(err, &ne) {
			continue
		}
		var ae *APIError
		if errors.As(err, &ae) && ae.Status >= 500 {
			continue
		}
		return nil, err
	}
}
