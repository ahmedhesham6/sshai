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
	tests := []struct {
		name          string
		configure     func(*runtimeWorkflowHarness)
		wantRoute     string
		wantReplace   bool
		wantErrorCode string
		wantStatus    domain.RuntimeStatus
		wantObserves  int
		wantStarts    int
		wantOpens     int
	}{
		{name: "happy start", wantRoute: "10.0.0.8", wantStatus: domain.RuntimeReady, wantObserves: 1, wantStarts: 1, wantOpens: 1},
		{name: "already ready short circuit", configure: func(h *runtimeWorkflowHarness) { h.actions.state.Runtime = readyRuntimeSnapshot() }, wantRoute: "10.0.0.7", wantStatus: domain.RuntimeReady},
		{name: "credit blocked", configure: func(h *runtimeWorkflowHarness) { h.credits.credits = 0 }, wantErrorCode: CreditsPolicyBlocked, wantStatus: domain.RuntimeStopped, wantObserves: 1},
		{name: "upgrade dispatches replace", configure: func(h *runtimeWorkflowHarness) { h.images.image = "image-v2" }, wantReplace: true, wantStatus: domain.RuntimeStopped, wantObserves: 1},
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
			if output.PrivateRoute != test.wantRoute || output.ReplaceDispatched != test.wantReplace {
				t.Fatalf("Runtime start output = %#v", output)
			}
			if got := harness.actions.runtimeStatus(); got != test.wantStatus {
				t.Fatalf("Runtime status = %q, want %q", got, test.wantStatus)
			}
			if harness.provider.observeCalls != test.wantObserves || harness.provider.startCalls != test.wantStarts || harness.usage.openCalls != test.wantOpens {
				t.Fatalf("provider observes / starts / usage opens = %d/%d/%d, want %d/%d/%d", harness.provider.observeCalls, harness.provider.startCalls, harness.usage.openCalls, test.wantObserves, test.wantStarts, test.wantOpens)
			}
			if test.wantReplace && (len(harness.sender.inputs) != 1 || harness.sender.inputs[0].OperationType != domain.OperationRuntimeReplace || harness.actions.decision != "replace:image-v2") {
				t.Fatalf("replace dispatch / decision = %#v / %q", harness.sender.inputs, harness.actions.decision)
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

type runtimeWorkflowHarness struct {
	actions  *runtimeActionsFake
	provider *runtimeProviderFake
	volume   *runtimeVolumeFake
	credits  *runtimeCreditsFake
	images   *runtimeImageFake
	usage    *runtimeUsageFake
	guest    *runtimeGuestFake
	auto     *runtimeAutoStopFake
	sender   *runtimeSenderFake
	clock    time.Time
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
		guest: &runtimeGuestFake{expectedOwnerID: "user-1"}, auto: &runtimeAutoStopFake{}, sender: &runtimeSenderFake{},
		clock: time.Date(2026, time.July, 18, 12, 5, 0, 0, time.UTC),
	}
}

func (h *runtimeWorkflowHarness) startDependencies() RuntimeStartDependencies {
	return RuntimeStartDependencies{
		Provider: h.provider, Actions: h.actions, DataVolumes: h.volume, Credits: h.credits, Images: h.images,
		Usage: h.usage, Guest: h.guest, SSHKeys: h.guest, AutoStop: h.auto, Replace: h.sender,
		IDs: fixedRuntimeID("usage-new"), Now: func() time.Time { return h.clock },
	}
}

func (h *runtimeWorkflowHarness) stopDependencies() RuntimeStopDependencies {
	return RuntimeStopDependencies{
		Provider: h.provider, Actions: h.actions, DataVolumes: h.volume, Snapshots: h.guest,
		Guest: h.guest, Usage: h.usage, AutoStop: h.auto, Now: func() time.Time { return h.clock },
	}
}

func runtimeDispatch(operationType domain.OperationType, reason domain.RuntimeStopReason) domain.RuntimeOperationDispatch {
	return domain.RuntimeOperationDispatch{
		OperationID: "operation-1", OperationType: operationType, EnvironmentID: "environment-1",
		RuntimeID: "runtime-1", OwnerUserID: "user-1", StopReason: reason,
	}
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

func (fake *runtimeActionsFake) runtimeStatus() domain.RuntimeStatus {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.state.Runtime.Status
}

type runtimeProviderFake struct {
	runtime                       provider.Runtime
	observeErr, startErr, stopErr error
	startCalls, stopCalls         int
	retireCalls, observeCalls     int
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
	fake.runtime.State, fake.runtime.PrivateIPv4 = provider.RuntimeStateRunning, "10.0.0.8"
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
	fake.runtime.State, fake.runtime.PrivateIPv4 = provider.RuntimeStateStopped, ""
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
	return fake.runtime, nil
}

type runtimeVolumeFake struct{ expectedOwnerID string }

func (fake *runtimeVolumeFake) VerifyRuntimeDataVolume(_ context.Context, request RuntimeDataVolumeRequest) error {
	if request.OwnerUserID != fake.expectedOwnerID || request.EnvironmentID != "environment-1" || request.RuntimeID != "runtime-1" || request.DataVolumeProviderID != "volume-1" {
		return errors.New("unexpected data volume ownership")
	}
	return nil
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
}

func (fake *runtimeUsageFake) OpenComputeUsageInterval(_ context.Context, input dbstore.OpenComputeUsageIntervalInput) (dbstore.ComputeUsageInterval, error) {
	if input.UserID != fake.expectedOwnerID {
		return dbstore.ComputeUsageInterval{}, errors.New("unexpected usage owner")
	}
	fake.openCalls++
	return dbstore.ComputeUsageInterval{ID: input.ID, UserID: input.UserID, EnvironmentID: input.EnvironmentID, RuntimeID: input.RuntimeID, StartedAt: input.StartedAt}, nil
}
func (fake *runtimeUsageFake) CloseComputeUsageInterval(_ context.Context, input dbstore.CloseComputeUsageIntervalInput) (billing.CreditTransaction, error) {
	fake.closeCalls++
	fake.closedInterval = input.IntervalID
	return billing.CreditTransaction{}, nil
}

type runtimeGuestFake struct {
	expectedOwnerID           string
	shutdownCalls, readyCalls int
	sshCalls, snapshotCalls   int
}

func (fake *runtimeGuestFake) WaitForRuntimeReady(_ context.Context, request RuntimeGuestReadinessRequest) (RuntimeGuestReadiness, error) {
	if request.OwnerUserID != fake.expectedOwnerID {
		return RuntimeGuestReadiness{}, errors.New("unexpected guest owner")
	}
	fake.readyCalls++
	return RuntimeGuestReadiness{BootID: "boot-new", PrivateIPv4: "10.0.0.8", DataMounted: true}, nil
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

type runtimeSenderFake struct {
	inputs []domain.RuntimeOperationDispatch
}

func (fake *runtimeSenderFake) SendRuntimeOperation(_ context.Context, input domain.RuntimeOperationDispatch) error {
	fake.inputs = append(fake.inputs, input)
	return nil
}

type fixedRuntimeID string

func (id fixedRuntimeID) NewID() string { return string(id) }
