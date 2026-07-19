// Package connection defines the wire protocol shared by the regional SSH
// proxy and the devm CLI for the pre-bridge phase of a connection attempt
// (spec 07). Before SSH bytes flow, the proxy may send JSON-encoded text
// frames describing start/readiness progress; after the terminal ready frame
// the WebSocket carries only binary SSH frames and text frames are invalid
// again, exactly as before this protocol existed.
package connection

import (
	"errors"
	"net/url"
)

// IntentHeader carries the opaque, single-use Connection Intent identity on
// the authenticated WebSocket upgrade request.
const IntentHeader = "X-Connection-Intent-ID"

// Step is the stable machine-readable vocabulary for connection progress and
// failure attribution shared by the proxy and CLI.
type Step string

const (
	StepStartingRuntime   Step = "starting-runtime"
	StepWaitingForGuest   Step = "waiting-for-guest"
	StepResolvingRoute    Step = "resolving-route"
	StepDialingRuntime    Step = "dialing-runtime"
	StepCreditsBlocked    Step = "credits-blocked"
	StepOperationConflict Step = "operation-conflict"
	StepClientProtocol    Step = "client-protocol"
	StepIntentInvalid     Step = "intent-invalid"
	StepReady             Step = "ready"
)

// ControlFrameType enumerates the pre-bridge text frames.
type ControlFrameType string

const (
	// ControlProgress reports a named step of the start/readiness wait.
	ControlProgress ControlFrameType = "progress"
	// ControlReady is the terminal control frame: the private route is
	// confirmed for the current boot and binary bridging begins after it.
	ControlReady ControlFrameType = "ready"
	// ControlFailed is the terminal control frame for a failed attempt; the
	// proxy closes the socket after sending it. Step names the failed step
	// per spec 07; Message must never contain SSH payload or token material.
	ControlFailed ControlFrameType = "failed"
)

// ControlFrame is the JSON body of a pre-bridge text frame.
type ControlFrame struct {
	Type ControlFrameType `json:"type"`
	// OperationID identifies the start Operation being awaited, when one
	// exists. Empty when the Runtime was already ready.
	OperationID string `json:"operationId,omitempty"`
	// Step is a stable machine-readable step name (e.g. "starting-runtime",
	// "waiting-for-guest", "resolving-route").
	Step Step `json:"step,omitempty"`
	// Message is a short human-readable description safe to print to a
	// user's terminal.
	Message string `json:"message,omitempty"`
}

// ProxyPath returns the shared regional proxy path for an Environment.
func ProxyPath(environmentID string) string {
	return "/v1/environments/" + url.PathEscape(environmentID) + "/ssh"
}

// ProxyURL builds an Environment-specific URL from a validated regional base.
func ProxyURL(base *url.URL, environmentID string) string {
	result := *base
	result.Path = ProxyPath(environmentID)
	result.RawPath = ""
	return result.String()
}

// ValidateProxyURL enforces the Connection Intent's regional WSS URL contract.
func ValidateProxyURL(value, environmentID string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "wss" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" ||
		parsed.Path != ProxyPath(environmentID) || parsed.RawPath != "" {
		return nil, errors.New("unsafe regional proxy URL")
	}
	return parsed, nil
}
