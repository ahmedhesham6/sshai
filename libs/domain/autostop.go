package domain

import (
	"errors"
	"fmt"
	"time"
)

type AutoStopMode string

const (
	AutoStopWhenDisconnected AutoStopMode = "when_disconnected"
	AutoStopWhenAgentsFinish AutoStopMode = "when_agents_finish"
	AutoStopWhenFullyIdle    AutoStopMode = "when_fully_idle"
	AutoStopManual           AutoStopMode = "manual"
)

type AutoStopPolicySnapshot struct {
	ID                 string
	EnvironmentID      string
	Mode               AutoStopMode
	GracePeriodSeconds int
}

type AutoStopPolicy struct {
	snapshot AutoStopPolicySnapshot
}

func NewAutoStopPolicy(id, environmentID string, mode AutoStopMode, gracePeriodSeconds int) (AutoStopPolicy, error) {
	if id == "" {
		return AutoStopPolicy{}, errors.New("create Auto-stop Policy: ID is required")
	}
	if environmentID == "" {
		return AutoStopPolicy{}, errors.New("create Auto-stop Policy: Environment ID is required")
	}
	switch mode {
	case AutoStopWhenDisconnected, AutoStopWhenAgentsFinish, AutoStopWhenFullyIdle, AutoStopManual:
	default:
		return AutoStopPolicy{}, fmt.Errorf("create Auto-stop Policy: unknown mode %q", mode)
	}
	if gracePeriodSeconds < 0 || gracePeriodSeconds > 86400 {
		return AutoStopPolicy{}, errors.New("create Auto-stop Policy: grace period must be between 0 and 86400 seconds")
	}
	return AutoStopPolicy{snapshot: AutoStopPolicySnapshot{
		ID:                 id,
		EnvironmentID:      environmentID,
		Mode:               mode,
		GracePeriodSeconds: gracePeriodSeconds,
	}}, nil
}

func (policy AutoStopPolicy) Snapshot() AutoStopPolicySnapshot {
	return policy.snapshot
}

type AutoStopConflict string

const (
	AutoStopConflictSetup           AutoStopConflict = "setup"
	AutoStopConflictMaterialization AutoStopConflict = "materialization"
	AutoStopConflictStart           AutoStopConflict = "start"
	AutoStopConflictReplace         AutoStopConflict = "replace"
	AutoStopConflictRestore         AutoStopConflict = "restore"
)

type AutoStopActivitySnapshot struct {
	RuntimeID            string
	Sequence             uint64
	ObservedAt           time.Time
	SSHConnections       int
	IDEConnections       int
	CodexProcesses       int
	ClaudeProcesses      int
	ProtectedProcesses   int
	SelectedContainers   int
	UnknownUserProcesses int
}

type AutoStopEvaluationRequest struct {
	RuntimeID                string
	PreviousSnapshotSequence uint64
	FreshAfter               time.Time
	Snapshot                 *AutoStopActivitySnapshot
	Conflicts                []AutoStopConflict
}

type AutoStopDecisionReason string

const (
	AutoStopQualifies              AutoStopDecisionReason = "qualifies"
	AutoStopBlockedActivity        AutoStopDecisionReason = "activity"
	AutoStopBlockedManual          AutoStopDecisionReason = "manual"
	AutoStopBlockedMissingSnapshot AutoStopDecisionReason = "missing_snapshot"
	AutoStopBlockedStaleSnapshot   AutoStopDecisionReason = "stale_snapshot"
	AutoStopBlockedStaleSequence   AutoStopDecisionReason = "stale_sequence"
	AutoStopBlockedRuntimeChanged  AutoStopDecisionReason = "runtime_changed"
	AutoStopBlockedConflict        AutoStopDecisionReason = "conflicting_operation"
)

type AutoStopDecision struct {
	Qualifies        bool
	Reason           AutoStopDecisionReason
	SnapshotSequence uint64
}

func (policy AutoStopPolicy) Evaluate(request AutoStopEvaluationRequest) (AutoStopDecision, error) {
	if policy.snapshot.Mode == AutoStopManual {
		return AutoStopDecision{Reason: AutoStopBlockedManual}, nil
	}
	if request.RuntimeID == "" || request.FreshAfter.IsZero() {
		return AutoStopDecision{}, errors.New("evaluate Auto-stop Policy: Runtime ID and freshness threshold are required")
	}
	if request.Snapshot == nil {
		return AutoStopDecision{Reason: AutoStopBlockedMissingSnapshot}, nil
	}
	snapshot := *request.Snapshot
	if err := validateAutoStopActivity(snapshot); err != nil {
		return AutoStopDecision{}, err
	}
	if snapshot.RuntimeID != request.RuntimeID {
		return AutoStopDecision{Reason: AutoStopBlockedRuntimeChanged, SnapshotSequence: snapshot.Sequence}, nil
	}
	if snapshot.Sequence <= request.PreviousSnapshotSequence {
		return AutoStopDecision{Reason: AutoStopBlockedStaleSequence, SnapshotSequence: snapshot.Sequence}, nil
	}
	if snapshot.ObservedAt.Before(request.FreshAfter) {
		return AutoStopDecision{Reason: AutoStopBlockedStaleSnapshot, SnapshotSequence: snapshot.Sequence}, nil
	}
	for _, conflict := range request.Conflicts {
		if !conflict.valid() {
			return AutoStopDecision{}, fmt.Errorf("evaluate Auto-stop Policy: unknown conflict %q", conflict)
		}
	}
	if len(request.Conflicts) > 0 {
		return AutoStopDecision{Reason: AutoStopBlockedConflict, SnapshotSequence: snapshot.Sequence}, nil
	}
	qualifies := false
	switch policy.snapshot.Mode {
	case AutoStopWhenDisconnected:
		qualifies = snapshot.SSHConnections == 0 && snapshot.IDEConnections == 0
	case AutoStopWhenAgentsFinish:
		qualifies = snapshot.CodexProcesses == 0 && snapshot.ClaudeProcesses == 0
	case AutoStopWhenFullyIdle:
		qualifies = snapshot.SSHConnections == 0 && snapshot.IDEConnections == 0 &&
			snapshot.CodexProcesses == 0 && snapshot.ClaudeProcesses == 0 &&
			snapshot.ProtectedProcesses == 0 && snapshot.SelectedContainers == 0 && snapshot.UnknownUserProcesses == 0
	}
	if !qualifies {
		return AutoStopDecision{Reason: AutoStopBlockedActivity, SnapshotSequence: snapshot.Sequence}, nil
	}
	return AutoStopDecision{Qualifies: true, Reason: AutoStopQualifies, SnapshotSequence: snapshot.Sequence}, nil
}

func validateAutoStopActivity(snapshot AutoStopActivitySnapshot) error {
	if snapshot.RuntimeID == "" || snapshot.Sequence == 0 || snapshot.ObservedAt.IsZero() {
		return errors.New("evaluate Auto-stop Policy: Activity Snapshot identity, sequence, and observation time are required")
	}
	counts := [...]int{snapshot.SSHConnections, snapshot.IDEConnections, snapshot.CodexProcesses, snapshot.ClaudeProcesses,
		snapshot.ProtectedProcesses, snapshot.SelectedContainers, snapshot.UnknownUserProcesses}
	for _, count := range counts {
		if count < 0 {
			return errors.New("evaluate Auto-stop Policy: Activity Snapshot counts cannot be negative")
		}
	}
	return nil
}

func (conflict AutoStopConflict) valid() bool {
	switch conflict {
	case AutoStopConflictSetup, AutoStopConflictMaterialization, AutoStopConflictStart, AutoStopConflictReplace, AutoStopConflictRestore:
		return true
	default:
		return false
	}
}
