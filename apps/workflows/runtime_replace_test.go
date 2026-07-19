//go:build !race

package workflows

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/provider"
	"github.com/ahmedhesham6/sshai/libs/testfixtures"
	"github.com/restatedev/sdk-go/ingress"
)

func TestRuntimeReplaceStoppedRuntimeReattachesOnlyAfterObservedDetachment(t *testing.T) {
	harness := newRuntimeReplaceHarness(false)
	output := runRuntimeReplace(t, harness, domain.OperationRuntimeReplace)

	if output.RetiredRuntimeID != "runtime-1" || output.ReplacementRuntimeID != "runtime-2" || output.PrivateRoute != "10.0.0.9" {
		t.Fatalf("Runtime replace output = %#v", output)
	}
	old := harness.actions.rows["runtime-1"]
	replacement := harness.actions.rows["runtime-2"]
	if old.Status != domain.RuntimeAbsent || old.RetiredAt == nil || replacement.Status != domain.RuntimeReady {
		t.Fatalf("retained Runtime rows = old %#v, replacement %#v", old, replacement)
	}
	if replacement.Sequence != 2 || replacement.AvailabilityZone != old.AvailabilityZone || replacement.ImageVersion != "image-v2" {
		t.Fatalf("replacement placement/image = %#v", replacement)
	}
	if harness.provider.stopCalls != 0 {
		t.Fatalf("stopped Runtime stop calls = %d, want 0", harness.provider.stopCalls)
	}
	retireIndex := eventIndex(harness.provider.events, "retire-old")
	detachedIndex := eventIndex(harness.provider.events, "observe-detached")
	ensureIndex := eventIndex(harness.provider.events, "ensure-new-rw")
	if retireIndex < 0 || detachedIndex <= retireIndex || ensureIndex <= detachedIndex {
		t.Fatalf("provider ordering = %v, want retire < detached observation < new RW attach", harness.provider.events)
	}
	if harness.usage.openCalls != 1 || harness.usage.openInputs[0].RuntimeID != "runtime-2" {
		t.Fatalf("replacement usage opens = %#v", harness.usage.openInputs)
	}
}

func TestRuntimeReplaceRestoresSSHHostIdentity(t *testing.T) {
	harness := newRuntimeReplaceHarness(false)
	runRuntimeReplace(t, harness, domain.OperationRuntimeReplace)
	if harness.guest.hostIdentityCalls != 1 || harness.guest.sshCalls != 1 || harness.guest.managedCalls != 1 {
		t.Fatalf("host identity / keys / managed calls = %d/%d/%d, want 1/1/1", harness.guest.hostIdentityCalls, harness.guest.sshCalls, harness.guest.managedCalls)
	}
}

func TestRuntimeReplaceFailuresFinalizeWithoutDeletingDataOrHistory(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*runtimeReplaceHarness)
		wantRows  int
	}{
		{name: "data health blocks retirement", configure: func(h *runtimeReplaceHarness) {
			h.volume.err = runtimePermanentError{error: errors.New("data unhealthy")}
		}, wantRows: 1},
		{name: "new compute failure retains retired and reserved rows", configure: func(h *runtimeReplaceHarness) {
			h.provider.ensureErr = provider.NewError(provider.ErrorCodeAuthorizationFailed, "denied", nil)
		}, wantRows: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newRuntimeReplaceHarness(false)
			test.configure(harness)
			environment := testfixtures.StartRestate(t, RuntimeReplaceDefinition(harness.dependencies()))
			input := runtimeDispatch(domain.OperationRuntimeReplace, "")
			if err := NewClient(environment.Ingress()).SendRuntimeOperation(t.Context(), input); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			_, err := ingress.WorkflowHandle[RuntimeReplaceOutput](environment.Ingress(), RuntimeReplaceService, input.OperationID).Attach(ctx)
			if err == nil || !strings.Contains(err.Error(), harness.actions.failureCode) {
				t.Fatalf("Runtime replace error = %v, failure code = %q", err, harness.actions.failureCode)
			}
			if harness.actions.completeCalls != 0 || harness.actions.failureCalls != 1 {
				t.Fatalf("Operation completions/failures = %d/%d", harness.actions.completeCalls, harness.actions.failureCalls)
			}
			if len(harness.actions.rows) != test.wantRows {
				t.Fatalf("retained Runtime rows = %d, want %d", len(harness.actions.rows), test.wantRows)
			}
			if harness.provider.deleteDataVolumeCalls != 0 {
				t.Fatalf("Data Volume delete calls = %d, want 0", harness.provider.deleteDataVolumeCalls)
			}
		})
	}
}

func TestRuntimeStartOutdatedImageIsFulfilledByOneReplacementOperation(t *testing.T) {
	harness := newRuntimeReplaceHarness(false)
	dependencies := RuntimeStartDependencies{
		Provider: harness.provider, Attachments: harness.provider, Actions: harness.actions,
		ReplacementActions: harness.actions, DataVolumes: harness.volume, Credits: &runtimeCreditsFake{expectedOwnerID: "user-1", credits: 10},
		Images: harness.images, Usage: harness.usage, Guest: harness.guest, HostIdentity: harness.guest,
		SSHKeys: harness.guest, Managed: harness.guest, AutoStop: harness.auto, IDs: harness.ids,
		Now: harness.now, ProviderPollInterval: time.Millisecond, ProviderPollTimeout: 10 * time.Second,
	}
	environment := testfixtures.StartRestate(t, RuntimeStartDefinition(dependencies))
	input := runtimeDispatch(domain.OperationRuntimeStart, "")
	if err := NewClient(environment.Ingress()).SendRuntimeOperation(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	output, err := ingress.WorkflowHandle[RuntimeStartOutput](environment.Ingress(), RuntimeStartService, input.OperationID).Attach(ctx)
	if err != nil {
		t.Fatalf("await upgraded Runtime start: %v", err)
	}
	if output.RuntimeID != "runtime-2" || output.PrivateRoute != "10.0.0.9" || harness.actions.current.ID != "runtime-2" || harness.actions.current.Status != domain.RuntimeReady {
		t.Fatalf("upgraded Runtime start = output %#v, current %#v", output, harness.actions.current)
	}
	if harness.actions.completeCalls != 1 || harness.actions.failureCalls != 0 || harness.actions.startDecision != "replace:image-v2" {
		t.Fatalf("single Operation completion/failure/decision = %d/%d/%q", harness.actions.completeCalls, harness.actions.failureCalls, harness.actions.startDecision)
	}
}

func TestRuntimeStartNeverInterruptsReadySessionForImageUpgrade(t *testing.T) {
	harness := newRuntimeReplaceHarness(true)
	dependencies := RuntimeStartDependencies{Actions: harness.actions, Images: harness.images, Now: harness.now}
	environment := testfixtures.StartRestate(t, RuntimeStartDefinition(dependencies))
	input := runtimeDispatch(domain.OperationRuntimeStart, "")
	if err := NewClient(environment.Ingress()).SendRuntimeOperation(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	output, err := ingress.WorkflowHandle[RuntimeStartOutput](environment.Ingress(), RuntimeStartService, input.OperationID).Attach(ctx)
	if err != nil || output.PrivateRoute != "10.0.0.7" {
		t.Fatalf("ready Runtime start = %#v, %v", output, err)
	}
	if harness.provider.stopCalls != 0 || harness.provider.retireCalls != 0 || harness.provider.ensureCalls != 0 {
		t.Fatalf("running session stop/retire/ensure calls = %d/%d/%d", harness.provider.stopCalls, harness.provider.retireCalls, harness.provider.ensureCalls)
	}
	if harness.images.calls != 0 {
		t.Fatalf("promoted image reads = %d, want 0 for ready short circuit", harness.images.calls)
	}
}

func TestRuntimeReplaceRunningRepairClosesUsageAtObservedStop(t *testing.T) {
	harness := newRuntimeReplaceHarness(true)
	runRuntimeReplace(t, harness, domain.OperationRuntimeReplace)
	if harness.provider.stopCalls != 1 || harness.usage.closeCalls != 1 || harness.usage.closedInterval != "usage-old" {
		t.Fatalf("repair stop/usage closure = %d/%d/%q", harness.provider.stopCalls, harness.usage.closeCalls, harness.usage.closedInterval)
	}
	if harness.usage.closedAt.IsZero() || harness.usage.closedSource != dbstore.ComputeUsageClosedByProviderReconciliation {
		t.Fatalf("repair usage closure observation/source = %v/%q", harness.usage.closedAt, harness.usage.closedSource)
	}
}

func runRuntimeReplace(t *testing.T, harness *runtimeReplaceHarness, operationType domain.OperationType) RuntimeReplaceOutput {
	t.Helper()
	environment := testfixtures.StartRestate(t, RuntimeReplaceDefinition(harness.dependencies()))
	input := runtimeDispatch(operationType, "")
	if err := NewClient(environment.Ingress()).SendRuntimeOperation(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	output, err := ingress.WorkflowHandle[RuntimeReplaceOutput](environment.Ingress(), RuntimeReplaceService, input.OperationID).Attach(ctx)
	if err != nil {
		t.Fatalf("await Runtime replace: %v", err)
	}
	return output
}

type runtimeReplaceHarness struct {
	actions  *runtimeReplaceActionsFake
	provider *runtimeReplaceProviderFake
	volume   *runtimeVolumeFake
	images   *runtimeReplaceImageFake
	usage    *runtimeUsageFake
	guest    *runtimeGuestFake
	auto     *runtimeAutoStopFake
	ids      *runtimeReplaceIDs
	clock    time.Time
}

func newRuntimeReplaceHarness(ready bool) *runtimeReplaceHarness {
	snapshot := stoppedRuntimeSnapshot()
	providerState := provider.RuntimeStateStopped
	usageID := ""
	if ready {
		snapshot = readyRuntimeSnapshot()
		providerState = provider.RuntimeStateRunning
		usageID = "usage-old"
	}
	actions := &runtimeReplaceActionsFake{state: RuntimeOperationState{OwnerUserID: "user-1", Runtime: snapshot, DataVolumeProviderID: "volume-1", ComputeUsageIntervalID: usageID}, current: snapshot, rows: map[string]domain.RuntimeSnapshot{"runtime-1": snapshot}}
	return &runtimeReplaceHarness{
		actions:  actions,
		provider: &runtimeReplaceProviderFake{old: provider.Runtime{RuntimeSpec: providerSpec(), ProviderID: "instance-1", PrivateIPv4: "10.0.0.7", State: providerState}},
		volume:   &runtimeVolumeFake{expectedOwnerID: "user-1"}, images: &runtimeReplaceImageFake{image: "image-v2"},
		usage: &runtimeUsageFake{expectedOwnerID: "user-1"}, guest: &runtimeGuestFake{expectedOwnerID: "user-1", readiness: RuntimeGuestReadiness{BootID: "boot-replacement", PrivateIPv4: "10.0.0.9", DataMounted: true}},
		auto: &runtimeAutoStopFake{}, ids: &runtimeReplaceIDs{values: []string{"runtime-2", "usage-new"}}, clock: time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC),
	}
}

func (h *runtimeReplaceHarness) dependencies() RuntimeReplaceDependencies {
	return RuntimeReplaceDependencies{
		Provider: h.provider, Attachments: h.provider, Actions: h.actions, DataVolumes: h.volume,
		Images: h.images, Usage: h.usage, Guest: h.guest, HostIdentity: h.guest, SSHKeys: h.guest,
		Managed: h.guest, AutoStop: h.auto, IDs: h.ids, Now: h.now,
		ProviderPollInterval: time.Millisecond, ProviderPollTimeout: 10 * time.Second,
	}
}

func (h *runtimeReplaceHarness) now() time.Time {
	value := h.clock
	h.clock = h.clock.Add(time.Second)
	return value
}

type runtimeReplaceActionsFake struct {
	state                       RuntimeOperationState
	current                     domain.RuntimeSnapshot
	rows                        map[string]domain.RuntimeSnapshot
	completeCalls, failureCalls int
	failureCode, startDecision  string
}

func (fake *runtimeReplaceActionsFake) LoadRuntimeOperation(context.Context, domain.RuntimeOperationDispatch, string, time.Time) (RuntimeOperationState, error) {
	fake.state.Runtime = fake.current
	return fake.state, nil
}
func (fake *runtimeReplaceActionsFake) PersistRuntimeTransition(_ context.Context, _ string, expected int64, next domain.RuntimeSnapshot) error {
	if fake.current.ID != next.ID || fake.current.Version != expected {
		return errors.New("stale old Runtime transition")
	}
	fake.current, fake.state.Runtime, fake.rows[next.ID] = next, next, next
	return nil
}
func (fake *runtimeReplaceActionsFake) PersistRuntimeReplacement(_ context.Context, _ string, owner string, expected int64, retired domain.RuntimeSnapshot, reservation domain.RuntimeReservation) (domain.RuntimeSnapshot, error) {
	if owner != "user-1" || fake.current.Version != expected || retired.ID != fake.current.ID || retired.Status != domain.RuntimeAbsent {
		return domain.RuntimeSnapshot{}, errors.New("invalid replacement persistence")
	}
	next, err := domain.ReserveRuntime(reservation)
	if err != nil {
		return domain.RuntimeSnapshot{}, err
	}
	fake.rows[retired.ID] = retired
	fake.current, fake.state.Runtime = next.Snapshot(), next.Snapshot()
	fake.rows[reservation.ID] = next.Snapshot()
	return next.Snapshot(), nil
}
func (fake *runtimeReplaceActionsFake) PersistReplacementRuntimeTransition(_ context.Context, _ string, expected int64, next domain.RuntimeSnapshot) error {
	if fake.current.ID != next.ID || fake.current.Version != expected {
		return errors.New("stale replacement Runtime transition")
	}
	fake.current, fake.state.Runtime, fake.rows[next.ID] = next, next, next
	return nil
}
func (fake *runtimeReplaceActionsFake) CompleteRuntimeOperation(context.Context, string, time.Time) error {
	fake.completeCalls++
	return nil
}
func (fake *runtimeReplaceActionsFake) RecordRuntimeFailure(_ context.Context, _ string, code, _ string, _ time.Time) error {
	fake.failureCalls++
	fake.failureCode = code
	return nil
}
func (fake *runtimeReplaceActionsFake) RecordRuntimeStartDecision(_ context.Context, _ string, decision, image string) error {
	fake.startDecision = decision + ":" + image
	return nil
}

type runtimeReplaceProviderFake struct {
	old                                    provider.Runtime
	new                                    provider.Runtime
	events                                 []string
	stopCalls, retireCalls, ensureCalls    int
	detachmentReads, deleteDataVolumeCalls int
	ensureErr                              error
}

func (fake *runtimeReplaceProviderFake) ObserveRuntime(_ context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	if request.RuntimeID == fake.old.RuntimeID {
		fake.events = append(fake.events, "observe-old")
		return fake.old, nil
	}
	if request.RuntimeID == fake.new.RuntimeID {
		fake.events = append(fake.events, "observe-new-running")
		fake.new.State, fake.new.PrivateIPv4 = provider.RuntimeStateRunning, "10.0.0.9"
		return fake.new, nil
	}
	return provider.Runtime{}, errors.New("unknown Runtime observation")
}
func (fake *runtimeReplaceProviderFake) StopRuntime(_ context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	fake.stopCalls++
	fake.events = append(fake.events, "stop-old")
	if request.ProviderID != fake.old.ProviderID {
		return provider.Runtime{}, errors.New("wrong old Runtime")
	}
	fake.old.State, fake.old.PrivateIPv4 = provider.RuntimeStateStopped, ""
	return fake.old, nil
}
func (fake *runtimeReplaceProviderFake) RetireRuntime(_ context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	fake.retireCalls++
	fake.events = append(fake.events, "retire-old")
	if request.ProviderID != fake.old.ProviderID {
		return provider.Runtime{}, errors.New("wrong old Runtime")
	}
	fake.old.State, fake.old.PrivateIPv4 = provider.RuntimeStateTerminated, ""
	return fake.old, nil
}
func (fake *runtimeReplaceProviderFake) ObserveRuntimeDataVolumeAttachment(context.Context, provider.RuntimeLifecycleRequest) (provider.RuntimeDataVolumeAttachment, error) {
	fake.detachmentReads++
	if fake.detachmentReads == 1 {
		fake.events = append(fake.events, "observe-attached")
		return provider.RuntimeDataVolumeAttachment{DataVolumeProviderID: "volume-1", RuntimeProviderID: "instance-1", Attached: true, ReadWrite: true}, nil
	}
	fake.events = append(fake.events, "observe-detached")
	return provider.RuntimeDataVolumeAttachment{DataVolumeProviderID: "volume-1"}, nil
}
func (fake *runtimeReplaceProviderFake) EnsureRuntime(_ context.Context, request provider.EnsureRuntimeRequest) (provider.Runtime, error) {
	fake.ensureCalls++
	if fake.detachmentReads < 2 {
		return provider.Runtime{}, errors.New("new RW attachment preceded old detachment observation")
	}
	fake.events = append(fake.events, "ensure-new-rw")
	if fake.ensureErr != nil {
		return provider.Runtime{}, fake.ensureErr
	}
	fake.new = provider.Runtime{RuntimeSpec: request.RuntimeSpec, ProviderID: "instance-2", State: provider.RuntimeStatePending}
	return fake.new, nil
}
func (fake *runtimeReplaceProviderFake) StartRuntime(context.Context, provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	return provider.Runtime{}, errors.New("replacement must boot from EnsureRuntime")
}

type runtimeReplaceImageFake struct {
	image string
	calls int
}

func (fake *runtimeReplaceImageFake) CurrentPromotedImage(context.Context, string) (string, error) {
	fake.calls++
	return fake.image, nil
}

type runtimeReplaceIDs struct {
	values []string
	index  int
}

func (ids *runtimeReplaceIDs) NewID() string {
	value := ids.values[ids.index]
	ids.index++
	return value
}

func eventIndex(events []string, want string) int {
	for index, event := range events {
		if event == want {
			return index
		}
	}
	return -1
}
