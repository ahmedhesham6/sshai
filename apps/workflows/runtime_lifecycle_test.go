//go:build !race

package workflows

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/billing"
	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/provider"
	"github.com/ahmedhesham6/sshai/libs/testfixtures"
	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"
)

var _ application.RuntimeOperationSender = (*Client)(nil)

type runtimePermanentError struct{ error }

func (runtimePermanentError) Transient() bool { return false }

type runtimeTransientError struct{ error }

func (runtimeTransientError) Transient() bool { return true }

func TestClientRoutesRuntimeReplace(t *testing.T) {
	definition := restate.NewWorkflow(RuntimeReplaceService).Handler(
		RunHandler,
		restate.NewWorkflowHandler(func(ctx restate.WorkflowContext, input domain.RuntimeOperationDispatch) (domain.RuntimeOperationDispatch, error) {
			if restate.Key(ctx) != input.OperationID {
				return domain.RuntimeOperationDispatch{}, restate.TerminalErrorf("wrong key")
			}
			return input, nil
		}),
	)
	environment := testfixtures.StartRestate(t, definition)
	input := runtimeDispatch(domain.OperationRuntimeReplace, "")
	if err := NewClient(environment.Ingress()).SendRuntimeOperation(t.Context(), input); err != nil {
		t.Fatalf("send Runtime replace: %v", err)
	}
	output, err := ingress.WorkflowHandle[domain.RuntimeOperationDispatch](environment.Ingress(), RuntimeReplaceService, input.OperationID).Attach(t.Context())
	if err != nil || output != input {
		t.Fatalf("Runtime replace routing = %#v, %v", output, err)
	}
}

func TestRuntimeStartWorkflow(t *testing.T) {
	providerAcceptedAt := time.Date(2026, time.July, 18, 12, 5, 0, 0, time.UTC)
	tests := []struct {
		name               string
		configure          func(*runtimeWorkflowHarness)
		wantRoute          string
		wantErrorCode      string
		wantStatus         domain.RuntimeStatus
		wantObserves       int
		wantStarts         int
		wantOpens          int
		wantFailure        string
		wantManaged        int
		wantUsageStartedAt *time.Time
	}{
		{name: "happy start", wantRoute: "10.0.0.8", wantStatus: domain.RuntimeReady, wantObserves: 1, wantStarts: 1, wantOpens: 1, wantManaged: 1},
		{name: "pending progresses to running", configure: func(h *runtimeWorkflowHarness) { h.provider.startPending = true }, wantRoute: "10.0.0.8", wantStatus: domain.RuntimeReady, wantObserves: 2, wantStarts: 1, wantOpens: 1, wantManaged: 1},
		{name: "already ready short circuit", configure: func(h *runtimeWorkflowHarness) { h.actions.state.Runtime = readyRuntimeSnapshot() }, wantRoute: "10.0.0.7", wantStatus: domain.RuntimeReady},
		{name: "credit blocked finalizes Operation", configure: func(h *runtimeWorkflowHarness) { h.credits.credits = 0 }, wantErrorCode: CreditsPolicyBlocked, wantFailure: CreditsPolicyBlocked, wantStatus: domain.RuntimeStopped, wantObserves: 1},
		{name: "upgrade requires future replace workflow", configure: func(h *runtimeWorkflowHarness) { h.images.image = "image-v2" }, wantErrorCode: ReplaceRequired, wantFailure: ReplaceRequired, wantStatus: domain.RuntimeStopped, wantObserves: 1},
		{name: "readiness failure marks Runtime error and keeps usage open", configure: func(h *runtimeWorkflowHarness) {
			h.guest.readiness = RuntimeGuestReadiness{BootID: "boot-new", PrivateIPv4: "10.0.0.8", DataMounted: false}
		}, wantErrorCode: GuestNotReady, wantFailure: GuestNotReady, wantStatus: domain.RuntimeError, wantObserves: 1, wantStarts: 1, wantOpens: 1},
		{name: "usage retry reuses journaled identity", configure: func(h *runtimeWorkflowHarness) {
			h.usage.openErrors = []error{runtimeTransientError{error: errors.New("retry usage")}}
		}, wantRoute: "10.0.0.8", wantStatus: domain.RuntimeReady, wantObserves: 1, wantStarts: 1, wantOpens: 2, wantManaged: 1},
		{name: "usage starts when provider accepts start", configure: func(h *runtimeWorkflowHarness) {
			h.ids = advancingRuntimeID{value: "usage-new", advance: func() { h.setClock(providerAcceptedAt.Add(time.Hour)) }}
		}, wantRoute: "10.0.0.8", wantStatus: domain.RuntimeReady, wantObserves: 1, wantStarts: 1, wantOpens: 1, wantManaged: 1, wantUsageStartedAt: &providerAcceptedAt},
		{name: "transient pending observations exhaust durable deadline", configure: func(h *runtimeWorkflowHarness) {
			h.provider.startPending = true
			h.provider.pollObserveErr = provider.NewError(provider.ErrorCodeUnavailable, "observation unavailable", nil)
			h.clockStep = 2 * time.Second
		}, wantErrorCode: string(provider.ErrorCodeUnavailable), wantFailure: string(provider.ErrorCodeUnavailable), wantStatus: domain.RuntimeError, wantObserves: 2, wantStarts: 1, wantOpens: 1},
		{name: "pre-start provider verification failure marks Runtime error", configure: func(h *runtimeWorkflowHarness) {
			h.provider.observeErr = provider.NewError(provider.ErrorCodeResourceDiverged, "instance missing", nil)
		}, wantErrorCode: string(provider.ErrorCodeResourceDiverged), wantStatus: domain.RuntimeError, wantObserves: 1},
		{name: "StartRuntime failure marks Runtime error", configure: func(h *runtimeWorkflowHarness) {
			h.provider.startErr = provider.NewError(provider.ErrorCodeAuthorizationFailed, "denied", nil)
		}, wantErrorCode: string(provider.ErrorCodeAuthorizationFailed), wantStatus: domain.RuntimeError, wantObserves: 1, wantStarts: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newRuntimeWorkflowHarness(false)
			if test.configure != nil {
				test.configure(harness)
			}
			environment := testfixtures.StartRestate(t, RuntimeStartDefinition(harness.startDependencies()))
			input := runtimeDispatch(domain.OperationRuntimeStart, domain.RuntimeStopReason(""))
			if err := NewClient(environment.Ingress()).SendRuntimeOperation(t.Context(), input); err != nil {
				t.Fatalf("send Runtime start: %v", err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			output, err := ingress.WorkflowHandle[RuntimeStartOutput](environment.Ingress(), RuntimeStartService, input.OperationID).Attach(ctx)
			if test.wantErrorCode != "" {
				if err == nil || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), test.wantErrorCode) {
					t.Fatalf("Runtime start error = %v, want terminal %s", err, test.wantErrorCode)
				}
			} else if err != nil {
				t.Fatalf("await Runtime start: %v", err)
			}
			if output.PrivateRoute != test.wantRoute {
				t.Fatalf("Runtime start output = %#v", output)
			}
			if got := harness.actions.runtimeStatus(); got != test.wantStatus {
				t.Fatalf("Runtime status = %q, want %q", got, test.wantStatus)
			}
			if harness.provider.observeCalls != test.wantObserves || harness.provider.startCalls != test.wantStarts || harness.usage.openCalls != test.wantOpens {
				t.Fatalf("provider observes / starts / usage opens = %d/%d/%d, want %d/%d/%d", harness.provider.observeCalls, harness.provider.startCalls, harness.usage.openCalls, test.wantObserves, test.wantStarts, test.wantOpens)
			}
			if test.wantErrorCode == ReplaceRequired && harness.actions.decision != "replace:image-v2" {
				t.Fatalf("replace decision = %q", harness.actions.decision)
			}
			if harness.actions.failureCode != test.wantFailure && test.wantFailure != "" {
				t.Fatalf("Operation failure = %q, want %q", harness.actions.failureCode, test.wantFailure)
			}
			if harness.guest.managedCalls != test.wantManaged {
				t.Fatalf("managed reconciliation calls = %d, want %d", harness.guest.managedCalls, test.wantManaged)
			}
			if test.name == "usage retry reuses journaled identity" && !harness.usage.sameOpenIdentity() {
				t.Fatalf("usage retry inputs changed: %#v", harness.usage.openInputs)
			}
			if test.wantUsageStartedAt != nil && (len(harness.usage.openInputs) != 1 || !harness.usage.openInputs[0].StartedAt.Equal(*test.wantUsageStartedAt)) {
				t.Fatalf("usage start = %#v, want %s", harness.usage.openInputs, test.wantUsageStartedAt.Format(time.RFC3339Nano))
			}
			if test.wantStatus == domain.RuntimeReady && harness.actions.completeCalls != 1 {
				t.Fatalf("completion calls = %d, want 1", harness.actions.completeCalls)
			}
		})
	}
}

func TestRuntimeStopWorkflowRecordsEveryReasonAndClosesUsage(t *testing.T) {
	for _, reason := range []domain.RuntimeStopReason{
		domain.RuntimeStopManual, domain.RuntimeStopAutoStop, domain.RuntimeStopBilling, domain.RuntimeStopRepair, domain.RuntimeStopResize,
	} {
		t.Run(string(reason), func(t *testing.T) {
			harness := newRuntimeWorkflowHarness(true)
			environment := testfixtures.StartRestate(t, RuntimeStopDefinition(harness.stopDependencies()))
			input := runtimeDispatch(domain.OperationRuntimeStop, reason)
			if err := NewClient(environment.Ingress()).SendRuntimeOperation(t.Context(), input); err != nil {
				t.Fatalf("send Runtime stop: %v", err)
			}
			output, err := ingress.WorkflowHandle[RuntimeStopOutput](environment.Ingress(), RuntimeStopService, input.OperationID).Attach(t.Context())
			if err != nil {
				t.Fatalf("await Runtime stop: %v", err)
			}
			if output.Reason != reason || harness.actions.reason != reason || harness.actions.runtimeStatus() != domain.RuntimeStopped {
				t.Fatalf("Runtime stop output/reason/status = %#v/%q/%q", output, harness.actions.reason, harness.actions.runtimeStatus())
			}
			if harness.provider.stopCalls != 1 || harness.provider.retireCalls != 0 || harness.usage.closeCalls != 1 || harness.usage.closedInterval != "usage-1" {
				t.Fatalf("stop/retire/close calls = %d/%d/%d interval %q", harness.provider.stopCalls, harness.provider.retireCalls, harness.usage.closeCalls, harness.usage.closedInterval)
			}
			if harness.auto.suppressCalls != 1 || harness.auto.resumeCalls != 0 || harness.actions.snapshotCalls != 1 || harness.guest.shutdownCalls != 1 {
				t.Fatalf("suppression/snapshot/shutdown = %d/%d/%d", harness.auto.suppressCalls, harness.actions.snapshotCalls, harness.guest.shutdownCalls)
			}
			if reason == domain.RuntimeStopAutoStop && (harness.actions.stopAudit == nil || len(harness.actions.stopAudit.QualifyingSnapshots) != 2) {
				t.Fatalf("Auto-stop audit = %#v", harness.actions.stopAudit)
			}
		})
	}
}

func TestRuntimeStopProviderFailureMarksRuntimeError(t *testing.T) {
	harness := newRuntimeWorkflowHarness(true)
	harness.provider.stopErr = provider.NewError(provider.ErrorCodeResourceDiverged, "wrong instance", nil)
	environment := testfixtures.StartRestate(t, RuntimeStopDefinition(harness.stopDependencies()))
	input := runtimeDispatch(domain.OperationRuntimeStop, domain.RuntimeStopRepair)
	if err := NewClient(environment.Ingress()).SendRuntimeOperation(t.Context(), input); err != nil {
		t.Fatalf("send Runtime stop: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := ingress.WorkflowHandle[RuntimeStopOutput](environment.Ingress(), RuntimeStopService, input.OperationID).Attach(ctx)
	if err == nil || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), string(provider.ErrorCodeResourceDiverged)) {
		t.Fatalf("Runtime stop error = %v", err)
	}
	if harness.actions.runtimeStatus() != domain.RuntimeError || harness.actions.failureCode != string(provider.ErrorCodeResourceDiverged) || harness.usage.closeCalls != 0 {
		t.Fatalf("failed Runtime status/code/close = %q/%q/%d", harness.actions.runtimeStatus(), harness.actions.failureCode, harness.usage.closeCalls)
	}
}

func TestRuntimeStopWaitsThroughStopping(t *testing.T) {
	harness := newRuntimeWorkflowHarness(true)
	harness.provider.stopStopping = true
	environment := testfixtures.StartRestate(t, RuntimeStopDefinition(harness.stopDependencies()))
	input := runtimeDispatch(domain.OperationRuntimeStop, domain.RuntimeStopManual)
	if err := NewClient(environment.Ingress()).SendRuntimeOperation(t.Context(), input); err != nil {
		t.Fatalf("send Runtime stop: %v", err)
	}
	if _, err := ingress.WorkflowHandle[RuntimeStopOutput](environment.Ingress(), RuntimeStopService, input.OperationID).Attach(t.Context()); err != nil {
		t.Fatalf("await Runtime stop: %v", err)
	}
	if harness.actions.runtimeStatus() != domain.RuntimeStopped || harness.provider.observeCalls != 1 || harness.usage.closeCalls != 1 {
		t.Fatalf("stopping progression status/observes/closes = %q/%d/%d", harness.actions.runtimeStatus(), harness.provider.observeCalls, harness.usage.closeCalls)
	}
}

func TestRuntimeStopVerificationFailureStillFinalizesPhysicalStop(t *testing.T) {
	harness := newRuntimeWorkflowHarness(true)
	harness.volume.err = runtimePermanentError{error: errors.New("persistent data missing")}
	environment := testfixtures.StartRestate(t, RuntimeStopDefinition(harness.stopDependencies()))
	input := runtimeDispatch(domain.OperationRuntimeStop, domain.RuntimeStopRepair)
	if err := NewClient(environment.Ingress()).SendRuntimeOperation(t.Context(), input); err != nil {
		t.Fatalf("send Runtime stop: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := ingress.WorkflowHandle[RuntimeStopOutput](environment.Ingress(), RuntimeStopService, input.OperationID).Attach(ctx)
	if err == nil || !strings.Contains(err.Error(), RuntimeStopFailed) {
		t.Fatalf("Runtime stop error = %v", err)
	}
	if harness.actions.runtimeStatus() != domain.RuntimeStopped || harness.usage.closeCalls != 1 || harness.actions.failureCode != RuntimeStopFailed {
		t.Fatalf("post-verification status/close/failure = %q/%d/%q", harness.actions.runtimeStatus(), harness.usage.closeCalls, harness.actions.failureCode)
	}
}

func TestRuntimeStopFailureReleasesAutoStopSuppression(t *testing.T) {
	harness := newRuntimeWorkflowHarness(true)
	harness.guest.snapshotErr = runtimePermanentError{error: errors.New("snapshot unavailable")}
	environment := testfixtures.StartRestate(t, RuntimeStopDefinition(harness.stopDependencies()))
	input := runtimeDispatch(domain.OperationRuntimeStop, domain.RuntimeStopManual)
	if err := NewClient(environment.Ingress()).SendRuntimeOperation(t.Context(), input); err != nil {
		t.Fatalf("send Runtime stop: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := ingress.WorkflowHandle[RuntimeStopOutput](environment.Ingress(), RuntimeStopService, input.OperationID).Attach(ctx)
	if err == nil || !strings.Contains(err.Error(), RuntimeStopFailed) {
		t.Fatalf("Runtime stop error = %v", err)
	}
	if harness.actions.runtimeStatus() != domain.RuntimeReady || harness.auto.suppressCalls != 1 || harness.auto.resumeCalls != 1 {
		t.Fatalf("failed stop status/suppress/resume = %q/%d/%d", harness.actions.runtimeStatus(), harness.auto.suppressCalls, harness.auto.resumeCalls)
	}
}

func TestRuntimeStopClosesUsageAtObservedStoppedTime(t *testing.T) {
	stoppedAt := time.Date(2026, time.July, 18, 12, 5, 0, 0, time.UTC)
	harness := newRuntimeWorkflowHarness(true)
	harness.setClock(stoppedAt)
	harness.volume.afterVerify = func() { harness.setClock(stoppedAt.Add(time.Hour)) }
	environment := testfixtures.StartRestate(t, RuntimeStopDefinition(harness.stopDependencies()))
	input := runtimeDispatch(domain.OperationRuntimeStop, domain.RuntimeStopManual)
	if err := NewClient(environment.Ingress()).SendRuntimeOperation(t.Context(), input); err != nil {
		t.Fatalf("send Runtime stop: %v", err)
	}
	if _, err := ingress.WorkflowHandle[RuntimeStopOutput](environment.Ingress(), RuntimeStopService, input.OperationID).Attach(t.Context()); err != nil {
		t.Fatalf("await Runtime stop: %v", err)
	}
	if !harness.usage.closedAt.Equal(stoppedAt) {
		t.Fatalf("usage stopped at %s, want provider observation %s", harness.usage.closedAt, stoppedAt)
	}
}

func TestRuntimeStopPollingDeadlineFinalizesOperation(t *testing.T) {
	harness := newRuntimeWorkflowHarness(true)
	harness.provider.stopStopping = true
	harness.provider.pollObserveErr = provider.NewError(provider.ErrorCodeUnavailable, "observation unavailable", nil)
	harness.clockStep = 2 * time.Second
	environment := testfixtures.StartRestate(t, RuntimeStopDefinition(harness.stopDependencies()))
	input := runtimeDispatch(domain.OperationRuntimeStop, domain.RuntimeStopManual)
	if err := NewClient(environment.Ingress()).SendRuntimeOperation(t.Context(), input); err != nil {
		t.Fatalf("send Runtime stop: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := ingress.WorkflowHandle[RuntimeStopOutput](environment.Ingress(), RuntimeStopService, input.OperationID).Attach(ctx)
	if err == nil || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), string(provider.ErrorCodeUnavailable)) {
		t.Fatalf("Runtime stop error = %v", err)
	}
	if harness.actions.runtimeStatus() != domain.RuntimeError || harness.actions.failureCode != string(provider.ErrorCodeUnavailable) {
		t.Fatalf("deadline status/failure = %q/%q", harness.actions.runtimeStatus(), harness.actions.failureCode)
	}
}

type runtimeWorkflowHarness struct {
	mu        sync.Mutex
	actions   *runtimeActionsFake
	provider  *runtimeProviderFake
	volume    *runtimeVolumeFake
	credits   *runtimeCreditsFake
	images    *runtimeImageFake
	usage     *runtimeUsageFake
	guest     *runtimeGuestFake
	auto      *runtimeAutoStopFake
	clock     time.Time
	clockStep time.Duration
	ids       IDGenerator
}

func newRuntimeWorkflowHarness(ready bool) *runtimeWorkflowHarness {
	snapshot := stoppedRuntimeSnapshot()
	providerState := provider.RuntimeStateStopped
	if ready {
		snapshot = readyRuntimeSnapshot()
		providerState = provider.RuntimeStateRunning
	}
	return &runtimeWorkflowHarness{
		actions: &runtimeActionsFake{expectedOwnerID: "user-1", state: RuntimeOperationState{
			OwnerUserID: "user-1", Runtime: snapshot, DataVolumeProviderID: "volume-1", ComputeUsageIntervalID: "usage-1",
		}},
		provider: &runtimeProviderFake{runtime: provider.Runtime{
			RuntimeSpec: providerSpec(), ProviderID: "instance-1", PrivateIPv4: "10.0.0.7", State: providerState,
		}},
		volume: &runtimeVolumeFake{expectedOwnerID: "user-1"}, credits: &runtimeCreditsFake{expectedOwnerID: "user-1", credits: 10},
		images: &runtimeImageFake{image: "image-v1"}, usage: &runtimeUsageFake{expectedOwnerID: "user-1"},
		guest: &runtimeGuestFake{expectedOwnerID: "user-1", readiness: RuntimeGuestReadiness{BootID: "boot-new", PrivateIPv4: "10.0.0.8", DataMounted: true}}, auto: &runtimeAutoStopFake{},
		clock: time.Date(2026, time.July, 18, 12, 5, 0, 0, time.UTC), ids: fixedRuntimeID("usage-new"),
	}
}

func (h *runtimeWorkflowHarness) startDependencies() RuntimeStartDependencies {
	return RuntimeStartDependencies{
		Provider: h.provider, Actions: h.actions, DataVolumes: h.volume, Credits: h.credits, Images: h.images,
		Usage: h.usage, Guest: h.guest, SSHKeys: h.guest, Managed: h.guest, AutoStop: h.auto,
		IDs: h.ids, Now: h.now,
		ProviderPollInterval: time.Millisecond, ProviderPollTimeout: time.Second,
	}
}

func (h *runtimeWorkflowHarness) stopDependencies() RuntimeStopDependencies {
	return RuntimeStopDependencies{
		Provider: h.provider, Actions: h.actions, DataVolumes: h.volume, Snapshots: h.guest,
		Guest: h.guest, Usage: h.usage, AutoStop: h.auto, Now: h.now,
		ProviderPollInterval: time.Millisecond, ProviderPollTimeout: time.Second,
	}
}

func (h *runtimeWorkflowHarness) now() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.clock
	h.clock = h.clock.Add(h.clockStep)
	return now
}

func (h *runtimeWorkflowHarness) setClock(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clock = now
}

func runtimeDispatch(operationType domain.OperationType, reason domain.RuntimeStopReason) domain.RuntimeOperationDispatch {
	dispatch := domain.RuntimeOperationDispatch{
		OperationID: "operation-1", OperationType: operationType, EnvironmentID: "environment-1",
		RuntimeID: "runtime-1", OwnerUserID: "user-1", StopReason: reason,
	}
	if reason == domain.RuntimeStopAutoStop {
		startedAt := time.Date(2026, time.July, 18, 12, 3, 0, 0, time.UTC)
		expiredAt := startedAt.Add(time.Minute)
		dispatch.StopAudit = &domain.RuntimeStopAuditEvidence{
			Policy:           domain.AutoStopPolicySnapshot{ID: "policy-1", EnvironmentID: "environment-1", Mode: domain.AutoStopWhenFullyIdle, GracePeriodSeconds: 60},
			PolicyGeneration: 3, GraceStartedAt: startedAt, GraceExpiredAt: expiredAt, GracePeriodSeconds: 60,
			QualifyingSnapshots: []domain.AutoStopActivitySnapshot{
				{RuntimeID: "runtime-1", Sequence: 10, ObservedAt: startedAt},
				{RuntimeID: "runtime-1", Sequence: 11, ObservedAt: expiredAt},
			},
		}
	}
	return dispatch
}

func stoppedRuntimeSnapshot() domain.RuntimeSnapshot {
	created := time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC)
	started, stopped := created.Add(time.Minute), created.Add(2*time.Minute)
	providerID := "instance-1"
	return domain.RuntimeSnapshot{
		ID: "runtime-1", EnvironmentID: "environment-1", Sequence: 1, Status: domain.RuntimeStopped,
		RuntimePreset: "cpu2-mem8", Region: "eu-central-1", AvailabilityZone: "eu-central-1a", ImageVersion: "image-v1",
		ProviderInstanceRef: &providerID, StartedAt: &started, StoppedAt: &stopped,
		CreatedAt: created, UpdatedAt: stopped, Version: 4,
	}
}

func readyRuntimeSnapshot() domain.RuntimeSnapshot {
	snapshot := stoppedRuntimeSnapshot()
	snapshot.Status = domain.RuntimeReady
	snapshot.StoppedAt = nil
	privateIPv4, bootID := "10.0.0.7", "boot-old"
	snapshot.PrivateAddress, snapshot.BootID = &privateIPv4, &bootID
	return snapshot
}

func providerSpec() provider.RuntimeSpec {
	return provider.RuntimeSpec{
		RuntimeID: "runtime-1", EnvironmentID: "environment-1", Sequence: 1, Region: "eu-central-1",
		AvailabilityZone: "eu-central-1a", RuntimePreset: "cpu2-mem8", ImageVersion: "image-v1", DataVolumeProviderID: "volume-1",
	}
}

type runtimeActionsFake struct {
	mu              sync.Mutex
	expectedOwnerID string
	state           RuntimeOperationState
	decision        string
	reason          domain.RuntimeStopReason
	snapshotCalls   int
	completeCalls   int
	failureCode     string
	stopAudit       *domain.RuntimeStopAuditEvidence
}

func (fake *runtimeActionsFake) LoadRuntimeOperation(_ context.Context, input domain.RuntimeOperationDispatch, invocationID string, _ time.Time) (RuntimeOperationState, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if input.OwnerUserID != fake.expectedOwnerID || invocationID == "" {
		return RuntimeOperationState{}, errors.New("unexpected Runtime owner or invocation")
	}
	return fake.state, nil
}

func (fake *runtimeActionsFake) PersistRuntimeTransition(_ context.Context, _ string, expectedVersion int64, next domain.RuntimeSnapshot) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.state.Runtime.Version != expectedVersion || next.EnvironmentID != fake.state.Runtime.EnvironmentID || next.ID != fake.state.Runtime.ID {
		return errors.New("unexpected Runtime transition ownership or version")
	}
	fake.state.Runtime = next
	return nil
}

func (fake *runtimeActionsFake) CompleteRuntimeOperation(context.Context, string, time.Time) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.completeCalls++
	return nil
}

func (fake *runtimeActionsFake) RecordRuntimeFailure(_ context.Context, _ string, code string, _ time.Time) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.failureCode = code
	return nil
}

func (fake *runtimeActionsFake) RecordRuntimeStartDecision(_ context.Context, _ string, decision, image string) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.decision = decision + ":" + image
	return nil
}

func (fake *runtimeActionsFake) RecordRuntimeStopReason(_ context.Context, _ string, reason domain.RuntimeStopReason) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.reason = reason
	return nil
}

func (fake *runtimeActionsFake) RecordRuntimeStopSnapshot(_ context.Context, _ string, _ AutoStopObservation) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.snapshotCalls++
	return nil
}

func (fake *runtimeActionsFake) RecordRuntimeStopAudit(_ context.Context, _ string, audit domain.RuntimeStopAuditEvidence) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.stopAudit = domain.CloneRuntimeStopAuditEvidence(&audit)
	return nil
}

func (fake *runtimeActionsFake) runtimeStatus() domain.RuntimeStatus {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.state.Runtime.Status
}

type runtimeProviderFake struct {
	runtime                                       provider.Runtime
	observeErr, pollObserveErr, startErr, stopErr error
	startCalls, stopCalls                         int
	retireCalls, observeCalls                     int
	startPending, stopStopping                    bool
}

func (fake *runtimeProviderFake) EnsureRuntime(context.Context, provider.EnsureRuntimeRequest) (provider.Runtime, error) {
	return provider.Runtime{}, errors.New("unexpected EnsureRuntime")
}
func (fake *runtimeProviderFake) StartRuntime(_ context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	fake.startCalls++
	if request.RuntimeSpec != fake.runtime.RuntimeSpec || request.ProviderID != fake.runtime.ProviderID {
		return provider.Runtime{}, errors.New("unexpected Runtime ownership")
	}
	if fake.startErr != nil {
		return provider.Runtime{}, fake.startErr
	}
	if fake.startPending {
		fake.runtime.State, fake.runtime.PrivateIPv4 = provider.RuntimeStatePending, ""
	} else {
		fake.runtime.State, fake.runtime.PrivateIPv4 = provider.RuntimeStateRunning, "10.0.0.8"
	}
	return fake.runtime, nil
}
func (fake *runtimeProviderFake) StopRuntime(_ context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	fake.stopCalls++
	if request.RuntimeSpec != fake.runtime.RuntimeSpec || request.ProviderID != fake.runtime.ProviderID {
		return provider.Runtime{}, errors.New("unexpected Runtime ownership")
	}
	if fake.stopErr != nil {
		return provider.Runtime{}, fake.stopErr
	}
	if fake.stopStopping {
		fake.runtime.State, fake.runtime.PrivateIPv4 = provider.RuntimeStateStopping, ""
	} else {
		fake.runtime.State, fake.runtime.PrivateIPv4 = provider.RuntimeStateStopped, ""
	}
	return fake.runtime, nil
}
func (fake *runtimeProviderFake) RetireRuntime(context.Context, provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	fake.retireCalls++
	return provider.Runtime{}, errors.New("Runtime stop must never retire")
}
func (fake *runtimeProviderFake) ObserveRuntime(_ context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	fake.observeCalls++
	if request.RuntimeSpec != fake.runtime.RuntimeSpec || request.ProviderID != fake.runtime.ProviderID {
		return provider.Runtime{}, errors.New("unexpected Runtime ownership")
	}
	if fake.observeErr != nil {
		return provider.Runtime{}, fake.observeErr
	}
	if fake.pollObserveErr != nil && (fake.runtime.State == provider.RuntimeStatePending || fake.runtime.State == provider.RuntimeStateStopping) {
		return provider.Runtime{}, fake.pollObserveErr
	}
	if fake.runtime.State == provider.RuntimeStatePending {
		fake.runtime.State, fake.runtime.PrivateIPv4 = provider.RuntimeStateRunning, "10.0.0.8"
	} else if fake.runtime.State == provider.RuntimeStateStopping {
		fake.runtime.State, fake.runtime.PrivateIPv4 = provider.RuntimeStateStopped, ""
	}
	return fake.runtime, nil
}

type runtimeVolumeFake struct {
	expectedOwnerID string
	err             error
	afterVerify     func()
}

func (fake *runtimeVolumeFake) VerifyRuntimeDataVolume(_ context.Context, request RuntimeDataVolumeRequest) error {
	if request.OwnerUserID != fake.expectedOwnerID || request.EnvironmentID != "environment-1" || request.RuntimeID != "runtime-1" || request.DataVolumeProviderID != "volume-1" {
		return errors.New("unexpected data volume ownership")
	}
	if fake.afterVerify != nil {
		fake.afterVerify()
	}
	return fake.err
}

type runtimeCreditsFake struct {
	expectedOwnerID string
	credits         int64
}

func (fake *runtimeCreditsFake) CreditBalance(_ context.Context, ownerUserID string) (dbstore.CreditBalanceProjection, error) {
	if ownerUserID != fake.expectedOwnerID {
		return dbstore.CreditBalanceProjection{}, errors.New("unexpected Credit Balance owner")
	}
	return dbstore.CreditBalanceProjection{UserID: ownerUserID, Credits: fake.credits}, nil
}

type runtimeImageFake struct{ image string }

func (fake *runtimeImageFake) CurrentPromotedImage(context.Context, string) (string, error) {
	return fake.image, nil
}

type runtimeUsageFake struct {
	expectedOwnerID       string
	openCalls, closeCalls int
	closedInterval        string
	closedAt              time.Time
	openInputs            []dbstore.OpenComputeUsageIntervalInput
	openErrors            []error
}

func (fake *runtimeUsageFake) OpenComputeUsageInterval(_ context.Context, input dbstore.OpenComputeUsageIntervalInput) (dbstore.ComputeUsageInterval, error) {
	if input.UserID != fake.expectedOwnerID {
		return dbstore.ComputeUsageInterval{}, errors.New("unexpected usage owner")
	}
	fake.openCalls++
	fake.openInputs = append(fake.openInputs, input)
	if len(fake.openErrors) > 0 {
		err := fake.openErrors[0]
		fake.openErrors = fake.openErrors[1:]
		return dbstore.ComputeUsageInterval{}, err
	}
	return dbstore.ComputeUsageInterval{ID: input.ID, UserID: input.UserID, EnvironmentID: input.EnvironmentID, RuntimeID: input.RuntimeID, StartedAt: input.StartedAt}, nil
}

func (fake *runtimeUsageFake) sameOpenIdentity() bool {
	if len(fake.openInputs) < 2 {
		return false
	}
	first := fake.openInputs[0]
	for _, input := range fake.openInputs[1:] {
		if input.ID != first.ID || !input.StartedAt.Equal(first.StartedAt) {
			return false
		}
	}
	return true
}
func (fake *runtimeUsageFake) CloseComputeUsageInterval(_ context.Context, input dbstore.CloseComputeUsageIntervalInput) (billing.CreditTransaction, error) {
	fake.closeCalls++
	fake.closedInterval = input.IntervalID
	fake.closedAt = input.StoppedAt
	return billing.CreditTransaction{}, nil
}

type runtimeGuestFake struct {
	expectedOwnerID           string
	shutdownCalls, readyCalls int
	sshCalls, snapshotCalls   int
	managedCalls              int
	readiness                 RuntimeGuestReadiness
	snapshotErr               error
}

func (fake *runtimeGuestFake) WaitForRuntimeReady(_ context.Context, request RuntimeGuestReadinessRequest) (RuntimeGuestReadiness, error) {
	if request.OwnerUserID != fake.expectedOwnerID {
		return RuntimeGuestReadiness{}, errors.New("unexpected guest owner")
	}
	fake.readyCalls++
	return fake.readiness, nil
}
func (fake *runtimeGuestFake) ReconcileRuntimeManagedConfiguration(_ context.Context, request RuntimeGuestReadinessRequest) error {
	if request.OwnerUserID != fake.expectedOwnerID {
		return errors.New("unexpected managed configuration owner")
	}
	fake.managedCalls++
	return nil
}
func (fake *runtimeGuestFake) ReconcileRuntimeSSHKeys(_ context.Context, request RuntimeGuestReadinessRequest) error {
	if request.OwnerUserID != fake.expectedOwnerID {
		return errors.New("unexpected SSH Key owner")
	}
	fake.sshCalls++
	return nil
}
func (fake *runtimeGuestFake) PrepareRuntimeShutdown(_ context.Context, request RuntimeGuestReadinessRequest) error {
	if request.OwnerUserID != fake.expectedOwnerID {
		return errors.New("unexpected shutdown owner")
	}
	fake.shutdownCalls++
	return nil
}
func (fake *runtimeGuestFake) RefreshAutoStop(_ context.Context, request AutoStopRefreshRequest) (AutoStopObservation, error) {
	fake.snapshotCalls++
	if fake.snapshotErr != nil {
		return AutoStopObservation{}, fake.snapshotErr
	}
	return AutoStopObservation{
		RuntimeID: request.RuntimeID,
		Snapshot:  &domain.AutoStopActivitySnapshot{RuntimeID: request.RuntimeID, Sequence: 1, ObservedAt: request.FreshAfter},
	}, nil
}

type runtimeAutoStopFake struct{ suppressCalls, resumeCalls int }

func (fake *runtimeAutoStopFake) SuppressAutoStop(context.Context, string, string) error {
	fake.suppressCalls++
	return nil
}
func (fake *runtimeAutoStopFake) ResumeAutoStop(context.Context, string, string) error {
	fake.resumeCalls++
	return nil
}

type fixedRuntimeID string

func (id fixedRuntimeID) NewID() string { return string(id) }

type advancingRuntimeID struct {
	value   string
	advance func()
}

func (id advancingRuntimeID) NewID() string {
	id.advance()
	return id.value
}
