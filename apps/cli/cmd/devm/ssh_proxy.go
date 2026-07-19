package main

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
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/connection"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/coder/websocket"
)

const (
	proxyRequestTimeout  = 15 * time.Second
	proxyBufferBytes     = 32 << 10
	proxyReadLimitBytes  = 1 << 20
	maxIntentBodyBytes   = 64 << 10
	connectionKeyPrefix  = "ssh-"
	connectionKeyHexSize = 24
)

type accessTokenSource interface {
	FreshAccessToken(context.Context) (string, error)
}

type sshProxyCommand struct {
	controlPlaneURL string
	httpClient      *http.Client
	tokens          accessTokenSource
	attempt         string
	input           io.Reader
	output          io.Writer
	errorOutput     io.Writer
	now             func() time.Time
}

func (command sshProxyCommand) run(ctx context.Context, environmentID string) error {
	if err := validateProxyCommand(command, environmentID); err != nil {
		return err
	}
	accessToken, err := command.tokens.FreshAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("authenticate SSH proxy: %w", err)
	}
	if accessToken == "" {
		return errors.New("authenticate SSH proxy: access token is unavailable")
	}
	client := cloneProxyHTTPClient(command.httpClient)
	api, err := contracts.NewClientWithResponses(command.controlPlaneURL, contracts.WithHTTPClient(boundedDoer{client: client}))
	if err != nil {
		return errors.New("configure SSH proxy: control plane URL is invalid")
	}
	idempotencyKey, err := connectionAttemptKey(environmentID, command.attempt)
	if err != nil {
		return err
	}
	requestContext, cancelRequest := context.WithTimeout(ctx, proxyRequestTimeout)
	response, err := api.CreateConnectionIntentWithResponse(
		requestContext,
		contracts.EnvironmentID(environmentID),
		&contracts.CreateConnectionIntentParams{IdempotencyKey: contracts.IdempotencyKey(idempotencyKey)},
		func(_ context.Context, request *http.Request) error {
			request.Header.Set("Authorization", "Bearer "+accessToken)
			return nil
		},
	)
	cancelRequest()
	if err != nil {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return errors.New("request SSH connection: control plane is unavailable")
	}
	if response.StatusCode() != http.StatusCreated || response.JSON201 == nil {
		return fmt.Errorf("request SSH connection: control plane returned HTTP %d", response.StatusCode())
	}
	proxyURL, err := validateConnectionIntent(*response.JSON201, environmentID, command.now())
	if err != nil {
		return err
	}
	dialContext, cancelDial := context.WithTimeout(ctx, proxyRequestTimeout)
	connection, _, err := websocket.Dial(dialContext, proxyURL.String(), &websocket.DialOptions{
		HTTPClient: client,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + accessToken}},
	})
	cancelDial()
	if err != nil {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return errors.New("open SSH connection: regional proxy is unavailable")
	}
	defer connection.CloseNow()
	connection.SetReadLimit(proxyReadLimitBytes)
	if err := consumeSSHControlFrames(ctx, connection, command.stderr(), accessToken); err != nil {
		return err
	}
	stream := websocket.NetConn(ctx, connection, websocket.MessageBinary)
	defer stream.Close()
	return copySSHStream(ctx, stream, command.input, command.output)
}

func (command sshProxyCommand) stderr() io.Writer {
	if command.errorOutput == nil {
		return io.Discard
	}
	return command.errorOutput
}

func consumeSSHControlFrames(ctx context.Context, socket *websocket.Conn, stderr io.Writer, bearer string) error {
	for {
		messageType, content, err := socket.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			return errors.New("prepare SSH connection: regional proxy closed before it was ready")
		}
		if messageType != websocket.MessageText {
			return errors.New("prepare SSH connection: protocol violation: binary frame received before ready")
		}
		var frame connection.ControlFrame
		if err := json.Unmarshal(content, &frame); err != nil {
			return errors.New("prepare SSH connection: regional proxy sent an invalid control frame")
		}
		switch frame.Type {
		case connection.ControlProgress:
			step := safeControlText(frame.Step, bearer)
			message := safeControlText(frame.Message, bearer)
			if step == "" && message == "" {
				continue
			}
			if step != "" && message != "" {
				_, err = fmt.Fprintf(stderr, "devm: %s: %s\n", step, message)
			} else {
				_, err = fmt.Fprintf(stderr, "devm: %s%s\n", step, message)
			}
			if err != nil {
				return errors.New("prepare SSH connection: write progress to stderr")
			}
		case connection.ControlReady:
			return nil
		case connection.ControlFailed:
			step := safeControlText(frame.Step, bearer)
			message := safeControlText(frame.Message, bearer)
			if step == "" {
				step = "unknown-step"
			}
			if message == "" {
				message = "the regional proxy could not prepare the Runtime"
			}
			return fmt.Errorf("SSH connection failed during %s: %s; persistent Environment state remains intact", step, message)
		default:
			// Unknown control types are ignored so newer proxies can add
			// advisory frames without breaking older CLIs.
			continue
		}
	}
}

func safeControlText(value, bearer string) string {
	const maximumRunes = 512
	value = strings.Map(func(character rune) rune {
		if character < 0x20 || character == 0x7f {
			return ' '
		}
		return character
	}, value)
	value = strings.TrimSpace(value)
	if bearer != "" {
		value = strings.ReplaceAll(value, bearer, "[redacted]")
	}
	runes := []rune(value)
	if len(runes) > maximumRunes {
		value = string(runes[:maximumRunes]) + "…"
	}
	return value
}

func validateProxyCommand(command sshProxyCommand, environmentID string) error {
	if !sshIdentifierPattern.MatchString(environmentID) {
		return errors.New("configure SSH proxy: Environment ID is invalid")
	}
	if command.tokens == nil || command.input == nil || command.output == nil || command.now == nil {
		return errors.New("configure SSH proxy: command is incomplete")
	}
	if _, err := secureControlPlaneURL(command.controlPlaneURL); err != nil {
		return err
	}
	if command.attempt == "" {
		return errors.New("configure SSH proxy: CLI attempt is unavailable")
	}
	return nil
}

func secureControlPlaneURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, errors.New("configure SSH proxy: HTTPS control plane URL without credentials, query, or fragment is required")
	}
	return parsed, nil
}

func connectionAttemptKey(environmentID, attempt string) (string, error) {
	if !sshIdentifierPattern.MatchString(environmentID) || strings.TrimSpace(attempt) == "" {
		return "", errors.New("configure SSH proxy: Environment and CLI attempt are required")
	}
	digest := sha256.Sum256([]byte(environmentID + "\x00" + attempt))
	return connectionKeyPrefix + hex.EncodeToString(digest[:])[:connectionKeyHexSize], nil
}

func validateConnectionIntent(intent contracts.ConnectionIntent, environmentID string, now time.Time) (*url.URL, error) {
	if intent.Id == "" || intent.EnvironmentId != environmentID || intent.LogicalHostname != environmentID || !intent.ExpiresAt.After(now) {
		return nil, errors.New("request SSH connection: control plane returned an invalid connection intent")
	}
	proxyURL, err := url.Parse(intent.ProxyUrl)
	expectedPath := "/v1/environments/" + environmentID + "/ssh"
	if err != nil || proxyURL.Scheme != "wss" || proxyURL.Host == "" || proxyURL.User != nil ||
		proxyURL.RawQuery != "" || proxyURL.ForceQuery || proxyURL.Fragment != "" || proxyURL.Path != expectedPath || proxyURL.RawPath != "" {
		return nil, errors.New("request SSH connection: control plane returned an unsafe regional proxy URL")
	}
	return proxyURL, nil
}

func cloneProxyHTTPClient(source *http.Client) *http.Client {
	if source == nil {
		source = http.DefaultClient
	}
	clone := *source
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &clone
}

type boundedDoer struct{ client *http.Client }

func (doer boundedDoer) Do(request *http.Request) (*http.Response, error) {
	response, err := doer.client.Do(request)
	if err != nil {
		return nil, err
	}
	response.Body = &boundedResponseBody{Reader: io.LimitReader(response.Body, maxIntentBodyBytes+1), Closer: response.Body}
	return response, nil
}

type boundedResponseBody struct {
	io.Reader
	io.Closer
}

type streamCopyResult struct {
	fromInput bool
	err       error
}

func copySSHStream(ctx context.Context, stream io.ReadWriteCloser, input io.Reader, output io.Writer) error {
	results := make(chan streamCopyResult, 2)
	go func() {
		_, err := io.CopyBuffer(stream, input, make([]byte, proxyBufferBytes))
		results <- streamCopyResult{fromInput: true, err: err}
	}()
	go func() {
		_, err := io.CopyBuffer(output, stream, make([]byte, proxyBufferBytes))
		results <- streamCopyResult{err: err}
	}()
	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case result := <-results:
			if result.fromInput && result.err == nil {
				// WebSockets have no transport half-close. Stop writing after local EOF
				// while retaining the read side until the peer closes or context ends.
				continue
			}
			if result.err == nil || websocket.CloseStatus(result.err) == websocket.StatusNormalClosure {
				return nil
			}
			if result.fromInput {
				return errors.New("stream SSH connection: local input failed")
			}
			return errors.New("stream SSH connection: regional proxy stream failed")
		}
	}
}
