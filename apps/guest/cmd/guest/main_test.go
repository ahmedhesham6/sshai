package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/apps/guest"
	guestcontrol "github.com/ahmedhesham6/sshai/apps/guest/control"
)

func TestSSHDIdentityActivatorReloadsBeforeHostKeyProbe(t *testing.T) {
	calls := make([]string, 0, 2)
	activator := sshdIdentityActivator{
		reloader: reloadRecorder{calls: &calls},
		prober:   probeRecorder{calls: &calls, expectedAddress: "10.0.0.8", expectedFingerprint: "SHA256:host"},
	}
	if err := activator.ActivateAndVerify(context.Background(), guestcontrol.Target{PrivateIPv4: "10.0.0.8"}, "SHA256:host"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "reload,probe" {
		t.Fatalf("activation order = %v, want reload then probe", calls)
	}
}

func TestTargetedFileSourcesRejectWrongBootAndSymlinks(t *testing.T) {
	target := guestcontrol.Target{
		OwnerUserID: "user-1", EnvironmentID: "environment-1", RuntimeID: "runtime-1",
		ProviderID: "instance-1", PrivateIPv4: "10.0.0.8",
	}
	path := filepath.Join(t.TempDir(), "keys.json")
	writeTargetedInput(t, path, targetedInput[[]guest.EnvironmentSSHKey]{
		Target: guestcontrol.Target{EnvironmentID: "environment-other"}, Value: []guest.EnvironmentSSHKey{},
	})
	if _, err := (jsonSSHKeySource{path: path}).SSHKeys(context.Background(), target); err == nil {
		t.Fatal("SSH key source accepted desired state for another boot")
	}
	symlink := filepath.Join(t.TempDir(), "keys-link.json")
	if err := os.Symlink(path, symlink); err != nil {
		t.Fatal(err)
	}
	var value any
	if err := decodePrivateJSONFile(symlink, &value, os.Geteuid()); err == nil {
		t.Fatal("private input reader followed a symbolic link")
	}
}

func TestActivityFileSourceRejectsStaleAndRegressedSamples(t *testing.T) {
	now := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	target := guestcontrol.Target{
		OwnerUserID: "user-1", EnvironmentID: "environment-1", RuntimeID: "runtime-1",
		ProviderID: "instance-1", PrivateIPv4: "10.0.0.8",
	}
	path := filepath.Join(t.TempDir(), "activity.json")
	source := &JSONActivitySampleSource{Path: path, Target: target, TrustedUID: os.Geteuid(), MaxAge: 5 * time.Minute, Now: func() time.Time { return now }}
	writeActivityInput(t, path, target, guest.ExternalSample{ObservedAt: now.Add(-6 * time.Minute), GuestSequence: 1, UserUID: 1000})
	if _, err := source.Sample(context.Background()); err == nil {
		t.Fatal("Activity source accepted stale sample")
	}
	writeActivityInput(t, path, target, guest.ExternalSample{ObservedAt: now, GuestSequence: 2, UserUID: 1000})
	if _, err := source.Sample(context.Background()); err != nil {
		t.Fatalf("accept current Activity sample: %v", err)
	}
	writeActivityInput(t, path, target, guest.ExternalSample{ObservedAt: now, GuestSequence: 1, UserUID: 1000})
	if _, err := source.Sample(context.Background()); err == nil {
		t.Fatal("Activity source accepted regressed sequence")
	}
}

func TestChownTreePreservesPrivateUmaskModes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "config")
	if err := os.WriteFile(file, []byte("private\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := chownTree(root, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("owned file mode/error = %v/%v, want 0600", info, err)
	}
}

func TestPrivateListenAddressRejectsPublicAndWildcardBindings(t *testing.T) {
	for _, address := range []string{"0.0.0.0:9443", "[::]:9443", "198.51.100.10:9443"} {
		if err := validatePrivateListenAddress(address); err == nil {
			t.Fatalf("validatePrivateListenAddress(%q) accepted non-private binding", address)
		}
	}
	for _, address := range []string{"10.0.0.8:9443", "127.0.0.1:9443"} {
		if err := validatePrivateListenAddress(address); err != nil {
			t.Fatalf("validatePrivateListenAddress(%q): %v", address, err)
		}
	}
}

func TestReadAgentRequirementsLoadsPackerVersionManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-versions")
	content := "claude\t/usr/local/bin/claude\t1.2.3\ncodex\t/usr/local/bin/codex\t4.5.6\nopencode\t/usr/local/bin/opencode\t7.8.9\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	requirements, err := readAgentRequirements(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(requirements) != 3 || requirements[0].Name != "claude" || requirements[1].ExpectedVersion != "4.5.6" || requirements[2].Executable != "/usr/local/bin/opencode" {
		t.Fatalf("agent requirements = %#v", requirements)
	}
}

type reloadRecorder struct{ calls *[]string }

func (recorder reloadRecorder) Reload(context.Context) error {
	*recorder.calls = append(*recorder.calls, "reload")
	return nil
}

type probeRecorder struct {
	calls               *[]string
	expectedAddress     string
	expectedFingerprint string
}

func (recorder probeRecorder) Probe(_ context.Context, address, fingerprint string) error {
	*recorder.calls = append(*recorder.calls, "probe")
	if address != recorder.expectedAddress || fingerprint != recorder.expectedFingerprint {
		return errors.New("probe received wrong identity")
	}
	return nil
}

func writeActivityInput(t *testing.T, path string, target guestcontrol.Target, sample guest.ExternalSample) {
	t.Helper()
	writeTargetedInput(t, path, targetedInput[guest.ExternalSample]{Target: target, Value: sample})
}

func writeTargetedInput(t *testing.T, path string, value any) {
	t.Helper()
	content, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}
