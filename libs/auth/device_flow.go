package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"
const workOSAPIURL = "https://api.workos.com"

type deviceFlowConfig struct {
	clientID   string
	apiURL     string
	httpClient *http.Client
	now        func() time.Time
	wait       func(context.Context, time.Duration) error
}

type DeviceFlow struct {
	clientID string
	apiURL   string
	client   *http.Client
	now      func() time.Time
	wait     func(context.Context, time.Duration) error
}

type DeviceAuthorization struct {
	deviceCode              string
	userCode                string
	verificationURI         string
	verificationURIComplete string
	expiresAt               time.Time
	interval                time.Duration
}

type DeviceTokens struct {
	accessToken  string
	refreshToken string
}

func NewDeviceFlow(clientID string) (*DeviceFlow, error) {
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return newDeviceFlow(deviceFlowConfig{clientID: clientID, apiURL: workOSAPIURL, httpClient: client})
}

func newDeviceFlow(config deviceFlowConfig) (*DeviceFlow, error) {
	if config.clientID == "" {
		return nil, errors.New("create WorkOS device flow: client ID is required")
	}
	if config.apiURL == "" {
		return nil, errors.New("create WorkOS device flow: API URL is required")
	}
	if _, err := url.ParseRequestURI(config.apiURL); err != nil {
		return nil, fmt.Errorf("create WorkOS device flow: invalid API URL: %w", err)
	}
	if config.httpClient == nil {
		config.httpClient = http.DefaultClient
	}
	if config.now == nil {
		config.now = time.Now
	}
	if config.wait == nil {
		config.wait = waitForDevicePoll
	}
	return &DeviceFlow{
		clientID: config.clientID, apiURL: strings.TrimRight(config.apiURL, "/"),
		client: config.httpClient, now: config.now, wait: config.wait,
	}, nil
}

func (flow *DeviceFlow) Authorize(ctx context.Context) (DeviceAuthorization, error) {
	var response struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int64  `json:"expires_in"`
		Interval                int64  `json:"interval"`
	}
	if err := flow.postForm(ctx, "/user_management/authorize/device", url.Values{
		"client_id": {flow.clientID},
	}, &response); err != nil {
		return DeviceAuthorization{}, fmt.Errorf("authorize WorkOS device: %w", err)
	}
	authorization := DeviceAuthorization{
		deviceCode: response.DeviceCode, userCode: response.UserCode,
		verificationURI: response.VerificationURI, verificationURIComplete: response.VerificationURIComplete,
		expiresAt: flow.now().Add(time.Duration(response.ExpiresIn) * time.Second),
		interval:  time.Duration(response.Interval) * time.Second,
	}
	if authorization.interval == 0 {
		authorization.interval = 5 * time.Second
	}
	if err := validateDeviceAuthorization(authorization); err != nil {
		return DeviceAuthorization{}, fmt.Errorf("authorize WorkOS device: %w", err)
	}
	return authorization, nil
}

func (flow *DeviceFlow) Poll(ctx context.Context, authorization DeviceAuthorization) (DeviceTokens, error) {
	if err := validateDeviceAuthorization(authorization); err != nil {
		return DeviceTokens{}, fmt.Errorf("poll WorkOS device authorization: %w", err)
	}
	interval := authorization.interval
	for {
		if !flow.now().Before(authorization.expiresAt) {
			return DeviceTokens{}, errors.New("poll WorkOS device authorization: authorization expired")
		}
		var response struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			Error        string `json:"error"`
		}
		status, err := flow.postFormStatus(ctx, "/user_management/authenticate", url.Values{
			"client_id": {flow.clientID}, "device_code": {authorization.deviceCode}, "grant_type": {deviceGrant},
		}, &response)
		if err != nil {
			return DeviceTokens{}, fmt.Errorf("poll WorkOS device authorization: %w", err)
		}
		if status >= 200 && status < 300 {
			if response.AccessToken == "" || response.RefreshToken == "" {
				return DeviceTokens{}, errors.New("poll WorkOS device authorization: token response is incomplete")
			}
			return DeviceTokens{accessToken: response.AccessToken, refreshToken: response.RefreshToken}, nil
		}
		switch response.Error {
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		case "access_denied", "expired_token":
			return DeviceTokens{}, fmt.Errorf("poll WorkOS device authorization: %s", response.Error)
		default:
			return DeviceTokens{}, errors.New("poll WorkOS device authorization: token endpoint rejected request")
		}
		if err := flow.wait(ctx, interval); err != nil {
			return DeviceTokens{}, fmt.Errorf("poll WorkOS device authorization: %w", err)
		}
	}
}

func validateDeviceAuthorization(authorization DeviceAuthorization) error {
	if authorization.deviceCode == "" || authorization.userCode == "" || authorization.verificationURI == "" || authorization.verificationURIComplete == "" {
		return errors.New("device authorization is incomplete")
	}
	if authorization.expiresAt.IsZero() || authorization.interval <= 0 {
		return errors.New("device authorization timing is invalid")
	}
	return nil
}

func (authorization DeviceAuthorization) UserCode() string { return authorization.userCode }

func (authorization DeviceAuthorization) VerificationURI() string {
	return authorization.verificationURI
}

func (authorization DeviceAuthorization) VerificationURIComplete() string {
	return authorization.verificationURIComplete
}

func (DeviceAuthorization) String() string   { return "WorkOS device authorization [redacted]" }
func (DeviceAuthorization) GoString() string { return "auth.DeviceAuthorization{[redacted]}" }

func (tokens DeviceTokens) AccessToken() string  { return tokens.accessToken }
func (tokens DeviceTokens) RefreshToken() string { return tokens.refreshToken }
func (DeviceTokens) String() string              { return "WorkOS device tokens [redacted]" }
func (DeviceTokens) GoString() string            { return "auth.DeviceTokens{[redacted]}" }

func (flow *DeviceFlow) postForm(ctx context.Context, path string, form url.Values, output any) error {
	status, err := flow.postFormStatus(ctx, path, form, output)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("WorkOS returned HTTP %d", status)
	}
	return nil
}

func (flow *DeviceFlow) postFormStatus(ctx context.Context, path string, form url.Values, output any) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, flow.apiURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := flow.client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(output); err != nil {
		return response.StatusCode, fmt.Errorf("decode WorkOS response: %w", err)
	}
	return response.StatusCode, nil
}

func waitForDevicePoll(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}
