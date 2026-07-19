package sshproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	startResponseLimit = 64 << 10
	startKeyPrefix     = "ssh-start-"
	startKeyDigestSize = 24
)

var (
	ErrCreditsPolicyBlocked = errors.New("Runtime start blocked by credit policy")
	ErrStartAuthorization   = errors.New("Runtime start authorization failed")
)

// RuntimeBootAttempt identifies the persisted state from which a start is
// requested. RuntimeVersion advances on every Runtime lifecycle transition.
type RuntimeBootAttempt struct {
	RuntimeID      string
	RuntimeVersion int64
	RuntimeStatus  string
}

type RuntimeBootAttemptSource interface {
	CurrentBootAttempt(context.Context, string) (RuntimeBootAttempt, error)
}

// ControlPlaneRuntimeStarter forwards the verified user bearer to the public
// start endpoint. Its idempotency key is SHA-256(Environment ID, Runtime ID,
// boot-attempt version), truncated to 96 bits and prefixed with "ssh-start-".
// The boot-attempt version is the stopped Runtime version, or version-1 after
// that Runtime enters starting. This normalizes both persisted states of one
// attempt to the same key; later stop/start cycles advance the version and
// receive a distinct key.
type ControlPlaneRuntimeStarter struct {
	baseURL  *url.URL
	client   *http.Client
	attempts RuntimeBootAttemptSource
}

func NewControlPlaneRuntimeStarter(baseURL string, client *http.Client, attempts RuntimeBootAttemptSource) (*ControlPlaneRuntimeStarter, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || attempts == nil {
		return nil, errors.New("create Runtime starter: HTTP control-plane base URL and boot-attempt source are required")
	}
	if client == nil {
		client = http.DefaultClient
	}
	clone := *client
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &ControlPlaneRuntimeStarter{baseURL: parsed, client: &clone, attempts: attempts}, nil
}

func (starter *ControlPlaneRuntimeStarter) EnsureStarted(ctx context.Context, bearer, environmentID string) (string, error) {
	if strings.TrimSpace(bearer) == "" || strings.TrimSpace(environmentID) == "" {
		return "", errors.New("request Runtime start: bearer and Environment ID are required")
	}
	attempt, err := starter.attempts.CurrentBootAttempt(ctx, environmentID)
	if err != nil {
		return "", errors.New("request Runtime start: boot attempt is unavailable")
	}
	key, err := runtimeStartKey(environmentID, attempt)
	if err != nil {
		return "", err
	}
	endpoint := *starter.baseURL
	basePath := strings.TrimRight(endpoint.Path, "/")
	if !strings.HasSuffix(basePath, "/v1") {
		basePath += "/v1"
	}
	endpoint.Path = basePath + "/environments/" + url.PathEscape(environmentID) + "/start"
	endpoint.RawPath = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), nil)
	if err != nil {
		return "", errors.New("request Runtime start: construct request")
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Idempotency-Key", key)
	request.Header.Set("Accept", "application/json")
	response, err := starter.client.Do(request)
	if err != nil {
		return "", errors.New("request Runtime start: control plane is unavailable")
	}
	defer response.Body.Close()

	var body struct {
		Operation struct {
			ID string `json:"id"`
		} `json:"operation"`
		Error struct {
			Code        string  `json:"code"`
			OperationID *string `json:"operationId"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, startResponseLimit+1)).Decode(&body); err != nil {
		return "", fmt.Errorf("request Runtime start: control plane returned HTTP %d", response.StatusCode)
	}
	operationID := body.Operation.ID
	if operationID == "" && body.Error.OperationID != nil {
		operationID = *body.Error.OperationID
	}
	switch {
	case response.StatusCode == http.StatusAccepted && operationID != "":
		return operationID, nil
	case response.StatusCode == http.StatusConflict && body.Error.Code == "OPERATION_CONFLICT" && attempt.RuntimeStatus == "starting":
		return operationID, nil
	case response.StatusCode == http.StatusUnauthorized || body.Error.Code == "AUTHORIZATION_FAILED":
		return "", ErrStartAuthorization
	case response.StatusCode == http.StatusForbidden && body.Error.Code == "CREDITS_POLICY_BLOCKED":
		return "", ErrCreditsPolicyBlocked
	default:
		return "", fmt.Errorf("request Runtime start: control plane returned HTTP %d", response.StatusCode)
	}
}

func runtimeStartKey(environmentID string, attempt RuntimeBootAttempt) (string, error) {
	if strings.TrimSpace(environmentID) == "" || strings.TrimSpace(attempt.RuntimeID) == "" || attempt.RuntimeVersion < 1 {
		return "", errors.New("request Runtime start: current boot attempt is invalid")
	}
	bootAttemptVersion := attempt.RuntimeVersion
	if attempt.RuntimeStatus == "starting" {
		bootAttemptVersion--
	}
	if bootAttemptVersion < 1 {
		return "", errors.New("request Runtime start: current boot attempt is invalid")
	}
	digest := sha256.Sum256([]byte(environmentID + "\x00" + attempt.RuntimeID + "\x00" + strconv.FormatInt(bootAttemptVersion, 10)))
	return startKeyPrefix + hex.EncodeToString(digest[:])[:startKeyDigestSize], nil
}
