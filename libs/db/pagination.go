package db

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// DefaultPageSize and MaxPageSize bound every owner-scoped collection read
// model in this package. They mirror api/openapi.yaml's PageSize parameter
// (minimum 1, maximum 100, default 50): the contract is the source of
// truth, these constants exist so store code does not hardcode the numbers
// twice.
const (
	DefaultPageSize = 50
	MaxPageSize     = 100
)

// ErrInvalidCursor reports a pagination cursor that does not decode to a
// well-formed keyset position: absent, corrupt, or truncated. Handlers
// translate it into a 400 response; it is never returned for a merely
// exhausted or unknown-but-well-formed cursor (an exhausted cursor simply
// yields an empty page).
var ErrInvalidCursor = permanent(errors.New("pagination cursor is invalid"))

// Cursor is the decoded keyset position a collection read model resumes
// from: the creation timestamp of the last item on the previous page, with
// its ID as a tiebreaker for rows that share that timestamp. Every List*
// query in this package orders by (created_at, id) and predicates on this
// exact tuple, so pages stay stable (no duplicates, no gaps) even when many
// rows share a creation instant.
//
// Cursors are opaque to callers. EncodeCursor/DecodeCursor round-trip a
// Cursor through base64(JSON) — the only cursor encoding used anywhere in
// this codebase (grepped before choosing it; nothing else exists to match).
// Clients must treat the encoded string as an opaque token.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

type cursorPayload struct {
	CreatedAt time.Time `json:"createdAt"`
	ID        string    `json:"id"`
}

// EncodeCursor renders a Cursor as the opaque string a client should echo
// back as the `cursor` query parameter to resume immediately after this
// position.
func EncodeCursor(cursor Cursor) string {
	raw, err := json.Marshal(cursorPayload{CreatedAt: cursor.CreatedAt.UTC(), ID: cursor.ID})
	if err != nil {
		// cursorPayload only contains a time.Time and a string; both always
		// marshal successfully. A failure here indicates a corrupted binary.
		panic("encode pagination cursor: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// DecodeCursor parses a client-supplied cursor string produced by
// EncodeCursor. A missing, malformed, or incomplete cursor reports
// ErrInvalidCursor.
func DecodeCursor(encoded string) (Cursor, error) {
	if encoded == "" {
		return Cursor{}, ErrInvalidCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return Cursor{}, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	var payload cursorPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Cursor{}, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	if payload.ID == "" || payload.CreatedAt.IsZero() {
		return Cursor{}, ErrInvalidCursor
	}
	return Cursor{CreatedAt: payload.CreatedAt, ID: payload.ID}, nil
}

// ClampPageSize applies this package's default/max page size to a
// caller-declared size: non-positive (including an absent size, reported as
// 0) reports DefaultPageSize; anything above MaxPageSize is clamped down to
// it. Every List* store method calls this so a bad size can never reach the
// database, regardless of what a caller above it already validated.
func ClampPageSize(size int) int {
	if size <= 0 {
		return DefaultPageSize
	}
	if size > MaxPageSize {
		return MaxPageSize
	}
	return size
}
