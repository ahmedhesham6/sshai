// Package connection defines the wire protocol shared by the regional SSH
// proxy and the devm CLI for the pre-bridge phase of a connection attempt
// (spec 07). Before SSH bytes flow, the proxy may send JSON-encoded text
// frames describing start/readiness progress; after the terminal ready frame
// the WebSocket carries only binary SSH frames and text frames are invalid
// again, exactly as before this protocol existed.
package connection

// IntentHeader carries the opaque, single-use Connection Intent identity on
// the authenticated WebSocket upgrade request.
const IntentHeader = "X-Connection-Intent-ID"

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
	Step string `json:"step,omitempty"`
	// Message is a short human-readable description safe to print to a
	// user's terminal.
	Message string `json:"message,omitempty"`
}
