package forge

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrDeviceFlowUnavailable is returned when OAuth device sign-in is not supported for a
// forge or no OAuth client ID is configured.
var ErrDeviceFlowUnavailable = errors.New("forge: device sign-in is not configured")

// DevicePollStatus is the outcome of one device-flow poll.
type DevicePollStatus string

const (
	DevicePending    DevicePollStatus = "pending"
	DeviceSlowDown   DevicePollStatus = "slow_down"
	DeviceAuthorized DevicePollStatus = "authorized"
	DeviceExpired    DevicePollStatus = "expired"
	DeviceDenied     DevicePollStatus = "denied"
)

// DeviceStartResult is returned when a device-code authorization flow starts.
type DeviceStartResult struct {
	DeviceCode              string    `json:"-"` // never expose to browsers
	UserCode                string    `json:"user_code"`
	VerificationURI         string    `json:"verification_uri"`
	VerificationURIComplete string    `json:"verification_uri_complete,omitempty"`
	ExpiresIn               int       `json:"expires_in"`
	Interval                int       `json:"interval"`
	ExpiresAt               time.Time `json:"expires_at"`
}

// DevicePollResult is the result of polling a device-code authorization flow.
type DevicePollResult struct {
	Status      DevicePollStatus `json:"status"`
	AccessToken string           `json:"-"` // never expose in API responses
	Interval    int              `json:"interval,omitempty"`
	Error       string           `json:"error,omitempty"`
}

// DeviceFlowClient implements the OAuth 2.0 Device Authorization Grant (RFC 8628) for
// GitHub and GitLab.
type DeviceFlowClient struct {
	GitHubClientID string
	GitLabClientID string
	HTTPClient     *http.Client
	Scheme         string           // default "https"
	Now            func() time.Time // default time.Now
}

// Available reports whether device flow is configured for kind.
func (d *DeviceFlowClient) Available(kind Kind) bool {
	if d == nil {
		return false
	}
	switch kind {
	case GitHub:
		return strings.TrimSpace(d.GitHubClientID) != ""
	case GitLab:
		return strings.TrimSpace(d.GitLabClientID) != ""
	default:
		return false
	}
}

func (d *DeviceFlowClient) clientIDFor(kind Kind) (string, error) {
	if d == nil {
		return "", fmt.Errorf("%w: %s", ErrDeviceFlowUnavailable, kind)
	}
	switch kind {
	case GitHub:
		id := strings.TrimSpace(d.GitHubClientID)
		if id == "" {
			return "", fmt.Errorf("%w: no GitHub OAuth client ID configured", ErrDeviceFlowUnavailable)
		}
		return id, nil
	case GitLab:
		id := strings.TrimSpace(d.GitLabClientID)
		if id == "" {
			return "", fmt.Errorf("%w: no GitLab OAuth client ID configured", ErrDeviceFlowUnavailable)
		}
		return id, nil
	default:
		return "", fmt.Errorf("%w: %s does not support device sign-in", ErrDeviceFlowUnavailable, kind)
	}
}

func (d *DeviceFlowClient) httpClient() *http.Client {
	if d != nil && d.HTTPClient != nil {
		return d.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (d *DeviceFlowClient) scheme() string {
	if d != nil && d.Scheme != "" {
		return d.Scheme
	}
	return "https"
}

func (d *DeviceFlowClient) now() time.Time {
	if d != nil && d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

// Start initiates an OAuth device flow on host for kind (GitHub or GitLab).
func (d *DeviceFlowClient) Start(ctx context.Context, host string, kind Kind) (DeviceStartResult, error) {
	clientID, err := d.clientIDFor(kind)
	if err != nil {
		return DeviceStartResult{}, err
	}
	host = NormalizeHost(host)
	if host == "" {
		return DeviceStartResult{}, errors.New("forge: host is required")
	}

	var endpoint, scope string
	switch kind {
	case GitHub:
		endpoint = d.scheme() + "://" + host + "/login/device/code"
		scope = "repo"
	case GitLab:
		endpoint = d.scheme() + "://" + host + "/oauth/authorize_device"
		scope = "api"
	}

	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("scope", scope)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return DeviceStartResult{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "konsert")

	resp, err := d.httpClient().Do(req)
	if err != nil {
		return DeviceStartResult{}, fmt.Errorf("forge: start device flow on %s: %w", host, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return DeviceStartResult{}, fmt.Errorf("forge: read device flow response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return DeviceStartResult{}, fmt.Errorf("forge: device flow start failed on %s: HTTP %d", host, resp.StatusCode)
	}

	var raw struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
		Error                   string `json:"error"`
		ErrorDescription        string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return DeviceStartResult{}, fmt.Errorf("forge: parse device flow response: %w", err)
	}
	if raw.Error != "" {
		msg := raw.Error
		if raw.ErrorDescription != "" {
			msg += ": " + raw.ErrorDescription
		}
		return DeviceStartResult{}, fmt.Errorf("forge: device flow start failed: %s", msg)
	}
	if raw.DeviceCode == "" || raw.UserCode == "" {
		return DeviceStartResult{}, errors.New("forge: incomplete device flow response")
	}
	if raw.Interval <= 0 {
		raw.Interval = 5
	}
	if raw.ExpiresIn <= 0 {
		raw.ExpiresIn = 900
	}
	return DeviceStartResult{
		DeviceCode:              raw.DeviceCode,
		UserCode:                raw.UserCode,
		VerificationURI:         raw.VerificationURI,
		VerificationURIComplete: raw.VerificationURIComplete,
		ExpiresIn:               raw.ExpiresIn,
		Interval:                raw.Interval,
		ExpiresAt:               d.now().Add(time.Duration(raw.ExpiresIn) * time.Second),
	}, nil
}

// Poll checks whether the user has authorized deviceCode. currentInterval is used to
// compute the bumped interval on slow_down.
func (d *DeviceFlowClient) Poll(ctx context.Context, host string, kind Kind, deviceCode string, currentInterval int, expiresAt time.Time) (DevicePollResult, error) {
	clientID, err := d.clientIDFor(kind)
	if err != nil {
		return DevicePollResult{}, err
	}
	if !expiresAt.IsZero() && !d.now().Before(expiresAt) {
		return DevicePollResult{Status: DeviceExpired, Error: "device code expired"}, nil
	}
	host = NormalizeHost(host)

	var endpoint string
	switch kind {
	case GitHub:
		endpoint = d.scheme() + "://" + host + "/login/oauth/access_token"
	case GitLab:
		endpoint = d.scheme() + "://" + host + "/oauth/token"
	}

	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("device_code", deviceCode)
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return DevicePollResult{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "konsert")

	resp, err := d.httpClient().Do(req)
	if err != nil {
		return DevicePollResult{}, fmt.Errorf("forge: poll device flow on %s: %w", host, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return DevicePollResult{}, fmt.Errorf("forge: read device poll response: %w", err)
	}

	var raw struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		Interval         int    `json:"interval"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		if resp.StatusCode >= 400 {
			return DevicePollResult{}, fmt.Errorf("forge: device flow poll failed on %s: HTTP %d", host, resp.StatusCode)
		}
		return DevicePollResult{}, fmt.Errorf("forge: parse device poll response: %w", err)
	}

	if raw.AccessToken != "" {
		return DevicePollResult{
			Status:      DeviceAuthorized,
			AccessToken: raw.AccessToken,
		}, nil
	}

	switch raw.Error {
	case "authorization_pending":
		return DevicePollResult{Status: DevicePending, Interval: currentInterval}, nil
	case "slow_down":
		next := raw.Interval
		if next <= currentInterval {
			if currentInterval <= 0 {
				currentInterval = 5
			}
			next = currentInterval + 5
		}
		return DevicePollResult{Status: DeviceSlowDown, Interval: next}, nil
	case "expired_token":
		return DevicePollResult{Status: DeviceExpired, Error: "device code expired"}, nil
	case "access_denied":
		return DevicePollResult{Status: DeviceDenied, Error: "authorization denied"}, nil
	default:
		msg := raw.Error
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		} else if raw.ErrorDescription != "" {
			msg += ": " + raw.ErrorDescription
		}
		return DevicePollResult{}, fmt.Errorf("forge: device flow poll error: %s", msg)
	}
}
