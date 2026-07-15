package providertest

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/ahmedhesham6/sshai/libs/provider"
)

type RuntimeHarness struct {
	Adapter                   provider.RuntimeProvider
	Request                   provider.EnsureRuntimeRequest
	AssertDataVolumePreserved func(*testing.T)
}

type RuntimeFactory func(*testing.T) RuntimeHarness

func RunRuntimeLifecycle(t *testing.T, factory RuntimeFactory) {
	t.Helper()
	harness := factory(t)
	ctx := t.Context()

	invalid := harness.Request
	invalid.RuntimeID = ""
	if _, err := harness.Adapter.EnsureRuntime(ctx, invalid); !hasCode(err, provider.ErrorCodeInvalidRequest) {
		t.Fatalf("EnsureRuntime() invalid request error = %v, want %s", err, provider.ErrorCodeInvalidRequest)
	}
	ensured, err := harness.Adapter.EnsureRuntime(ctx, harness.Request)
	if err != nil {
		t.Fatalf("EnsureRuntime(): %v", err)
	}
	assertRuntimeOwnership(t, ensured, harness.Request.RuntimeSpec)
	replayed, err := harness.Adapter.EnsureRuntime(ctx, harness.Request)
	if err != nil {
		t.Fatalf("EnsureRuntime() replay: %v", err)
	}
	assertSameRuntime(t, replayed, ensured)

	request := provider.RuntimeLifecycleRequest{RuntimeSpec: harness.Request.RuntimeSpec, ProviderID: ensured.ProviderID}
	diverged := request
	diverged.Sequence++
	if _, err := harness.Adapter.ObserveRuntime(ctx, diverged); !hasCode(err, provider.ErrorCodeResourceDiverged) {
		t.Fatalf("ObserveRuntime() divergence error = %v, want %s", err, provider.ErrorCodeResourceDiverged)
	}
	running, err := harness.Adapter.StartRuntime(ctx, request)
	if err != nil {
		t.Fatalf("StartRuntime(): %v", err)
	}
	if running.State != provider.RuntimeStateRunning || !privateIPv4(running.PrivateIPv4) {
		t.Fatalf("started Runtime = %#v", running)
	}
	replayed, err = harness.Adapter.StartRuntime(ctx, request)
	if err != nil {
		t.Fatalf("StartRuntime() replay: %v", err)
	}
	assertSameRuntime(t, replayed, running)
	observed, err := harness.Adapter.ObserveRuntime(ctx, request)
	if err != nil {
		t.Fatalf("ObserveRuntime(): %v", err)
	}
	assertSameRuntime(t, observed, running)

	stopped, err := harness.Adapter.StopRuntime(ctx, request)
	if err != nil {
		t.Fatalf("StopRuntime(): %v", err)
	}
	if stopped.State != provider.RuntimeStateStopped || stopped.PrivateIPv4 != "" {
		t.Fatalf("stopped Runtime = %#v", stopped)
	}
	replayed, err = harness.Adapter.StopRuntime(ctx, request)
	if err != nil {
		t.Fatalf("StopRuntime() replay: %v", err)
	}
	assertSameRuntime(t, replayed, stopped)

	running, err = harness.Adapter.StartRuntime(ctx, request)
	if err != nil {
		t.Fatalf("restart Runtime: %v", err)
	}
	if running.State != provider.RuntimeStateRunning || !privateIPv4(running.PrivateIPv4) {
		t.Fatalf("restarted Runtime = %#v", running)
	}
	if _, err := harness.Adapter.StopRuntime(ctx, request); err != nil {
		t.Fatalf("stop Runtime before retirement: %v", err)
	}
	retired, err := harness.Adapter.RetireRuntime(ctx, request)
	if err != nil {
		t.Fatalf("RetireRuntime(): %v", err)
	}
	if retired.State != provider.RuntimeStateTerminated || retired.PrivateIPv4 != "" {
		t.Fatalf("retired Runtime = %#v", retired)
	}
	replayed, err = harness.Adapter.RetireRuntime(ctx, request)
	if err != nil {
		t.Fatalf("RetireRuntime() replay: %v", err)
	}
	assertSameRuntime(t, replayed, retired)
	observed, err = harness.Adapter.ObserveRuntime(ctx, request)
	if err != nil {
		t.Fatalf("observe retired Runtime: %v", err)
	}
	assertSameRuntime(t, observed, retired)
	if harness.AssertDataVolumePreserved != nil {
		harness.AssertDataVolumePreserved(t)
	}
}

func assertRuntimeOwnership(t *testing.T, runtime provider.Runtime, spec provider.RuntimeSpec) {
	t.Helper()
	if runtime.ProviderID == "" || runtime.RuntimeID != spec.RuntimeID || runtime.EnvironmentID != spec.EnvironmentID ||
		runtime.Sequence != spec.Sequence || runtime.Region != spec.Region || runtime.AvailabilityZone != spec.AvailabilityZone ||
		runtime.RuntimePreset != spec.RuntimePreset || runtime.ImageVersion != spec.ImageVersion ||
		runtime.DataVolumeProviderID != spec.DataVolumeProviderID || (runtime.PrivateIPv4 != "" && !privateIPv4(runtime.PrivateIPv4)) {
		t.Fatalf("Runtime ownership = %#v, want %#v", runtime, spec)
	}
}

func hasCode(err error, code provider.ErrorCode) bool {
	var providerError *provider.Error
	return errors.As(err, &providerError) && providerError.Code == code
}

func privateIPv4(value string) bool {
	address, err := netip.ParseAddr(value)
	return err == nil && address.Is4() && address.IsPrivate()
}

func assertSameRuntime(t *testing.T, got, want provider.Runtime) {
	t.Helper()
	if got != want {
		t.Fatalf("Runtime = %#v, want %#v", got, want)
	}
}
