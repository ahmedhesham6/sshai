package guest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var ErrStaleBootReadiness = errors.New("stale boot readiness")

type ReadinessLevel string

const (
	ReadinessAllocated       ReadinessLevel = "allocated"
	ReadinessDataMounted     ReadinessLevel = "data_mounted"
	ReadinessSSHReady        ReadinessLevel = "ssh_ready"
	ReadinessProjectReady    ReadinessLevel = "project_ready"
	ReadinessAgentsValidated ReadinessLevel = "agents_validated"
)

type BootIdentity struct {
	RuntimeID       string
	BootID          string
	RuntimeSequence int64
}

type CurrentBootSource interface {
	CurrentBoot(context.Context) (BootIdentity, error)
}

type ReadinessSnapshot struct {
	RuntimeID       string         `json:"runtime_id"`
	BootID          string         `json:"boot_id"`
	RuntimeSequence int64          `json:"runtime_sequence"`
	Level           ReadinessLevel `json:"level"`
	ObservedAt      time.Time      `json:"observed_at"`
}

type ReadinessReporter struct {
	source   CurrentBootSource
	identity BootIdentity
	mu       sync.Mutex
	snapshot ReadinessSnapshot
}

func NewReadinessReporter(ctx context.Context, identity BootIdentity, source CurrentBootSource, allocatedAt time.Time) (*ReadinessReporter, ReadinessSnapshot, error) {
	if source == nil {
		return nil, ReadinessSnapshot{}, errors.New("create readiness reporter: current boot source is required")
	}
	if err := validateBootIdentity(identity); err != nil {
		return nil, ReadinessSnapshot{}, fmt.Errorf("create readiness reporter: %w", err)
	}
	if allocatedAt.IsZero() {
		return nil, ReadinessSnapshot{}, errors.New("create readiness reporter: allocation time is required")
	}
	current, err := source.CurrentBoot(ctx)
	if err != nil {
		return nil, ReadinessSnapshot{}, fmt.Errorf("create readiness reporter: inspect current boot: %w", err)
	}
	if err := validateBootIdentity(current); err != nil {
		return nil, ReadinessSnapshot{}, fmt.Errorf("create readiness reporter: current boot: %w", err)
	}
	if current != identity {
		return nil, ReadinessSnapshot{}, ErrStaleBootReadiness
	}
	snapshot := ReadinessSnapshot{
		RuntimeID: identity.RuntimeID, BootID: identity.BootID, RuntimeSequence: identity.RuntimeSequence,
		Level: ReadinessAllocated, ObservedAt: allocatedAt.Round(0).UTC(),
	}
	return &ReadinessReporter{source: source, identity: identity, snapshot: snapshot}, snapshot, nil
}

func (reporter *ReadinessReporter) Advance(ctx context.Context, level ReadinessLevel, observedAt time.Time) (ReadinessSnapshot, error) {
	if err := reporter.validateCurrentBoot(ctx); err != nil {
		return ReadinessSnapshot{}, err
	}
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	if observedAt.IsZero() || observedAt.Before(reporter.snapshot.ObservedAt) {
		return ReadinessSnapshot{}, errors.New("advance readiness: observation time precedes current readiness")
	}
	currentOrder, nextOrder := readinessOrder(reporter.snapshot.Level), readinessOrder(level)
	if nextOrder == 0 {
		return ReadinessSnapshot{}, fmt.Errorf("advance readiness: unknown level %q", level)
	}
	if nextOrder == currentOrder {
		return reporter.snapshot, nil
	}
	if nextOrder != currentOrder+1 {
		return ReadinessSnapshot{}, fmt.Errorf("advance readiness: level %q cannot follow %q", level, reporter.snapshot.Level)
	}
	reporter.snapshot.Level = level
	reporter.snapshot.ObservedAt = observedAt.Round(0).UTC()
	return reporter.snapshot, nil
}

// CompareReadiness orders two readiness levels from allocation through agent
// validation. Unknown levels sort before every valid readiness level.
func CompareReadiness(left, right ReadinessLevel) int {
	return readinessOrder(left) - readinessOrder(right)
}

func (reporter *ReadinessReporter) Snapshot(ctx context.Context) (ReadinessSnapshot, error) {
	if err := reporter.validateCurrentBoot(ctx); err != nil {
		return ReadinessSnapshot{}, err
	}
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	return reporter.snapshot, nil
}

func (reporter *ReadinessReporter) validateCurrentBoot(ctx context.Context) error {
	if reporter == nil || reporter.source == nil {
		return errors.New("readiness reporter is not initialized")
	}
	current, err := reporter.source.CurrentBoot(ctx)
	if err != nil {
		return fmt.Errorf("inspect current boot: %w", err)
	}
	if current != reporter.identity {
		return ErrStaleBootReadiness
	}
	return nil
}

func validateBootIdentity(identity BootIdentity) error {
	if strings.TrimSpace(identity.RuntimeID) == "" || strings.TrimSpace(identity.BootID) == "" || identity.RuntimeSequence < 1 {
		return errors.New("Runtime ID, boot ID, and positive Runtime sequence are required")
	}
	return nil
}

func readinessOrder(level ReadinessLevel) int {
	switch level {
	case ReadinessAllocated:
		return 1
	case ReadinessDataMounted:
		return 2
	case ReadinessSSHReady:
		return 3
	case ReadinessProjectReady:
		return 4
	case ReadinessAgentsValidated:
		return 5
	default:
		return 0
	}
}
