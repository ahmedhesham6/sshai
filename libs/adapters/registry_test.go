package adapters

import (
	"strings"
	"testing"
)

func TestRegisteredAdapters(t *testing.T) {
	for _, id := range []string{"claude", "codex", "opencode"} {
		adapter, err := For(id)
		if err != nil {
			t.Fatalf("For(%q) error = %v", id, err)
		}
		if adapter.ID() != id {
			t.Fatalf("For(%q).ID() = %q, want %q", id, adapter.ID(), id)
		}
		if adapter.Version() == "" {
			t.Fatalf("For(%q).Version() is empty", id)
		}
	}
	_, err := For("unknown-agent")
	if err == nil {
		t.Fatal("For(\"unknown-agent\") error = nil, want error")
	}
	if !strings.Contains(err.Error(), `unknown capsule component adapter "unknown-agent"`) {
		t.Fatalf("For(\"unknown-agent\") error = %q, want unknown capsule component adapter message", err)
	}
}
