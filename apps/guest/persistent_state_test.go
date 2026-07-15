package guest_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ahmedhesham6/sshai/apps/guest"
)

type staticMountInspector struct {
	mount guest.PersistentMount
	err   error
}

func (inspector staticMountInspector) InspectPersistentMount(context.Context, string) (guest.PersistentMount, error) {
	return inspector.mount, inspector.err
}

func TestBootstrapPersistentStateCreatesExactDurableLayoutIdempotently(t *testing.T) {
	root := t.TempDir()
	request := persistentStateRequest(root)
	inspector := staticMountInspector{mount: persistentMount(root)}

	first, err := guest.BootstrapPersistentState(t.Context(), request, inspector)
	if err != nil {
		t.Fatalf("BootstrapPersistentState(): %v", err)
	}
	second, err := guest.BootstrapPersistentState(t.Context(), request, inspector)
	if err != nil {
		t.Fatalf("BootstrapPersistentState() replay: %v", err)
	}
	if second != first {
		t.Fatalf("replayed layout = %#v, want %#v", second, first)
	}
	wantPaths := []string{
		filepath.Join(root, "cache"), filepath.Join(root, "home"), filepath.Join(root, "platform"),
		filepath.Join(root, "services"), filepath.Join(root, "workspace"),
	}
	gotPaths, err := filepath.Glob(filepath.Join(root, "*"))
	if err != nil {
		t.Fatalf("list layout: %v", err)
	}
	slices.Sort(gotPaths)
	if !slices.Equal(gotPaths, wantPaths) {
		t.Fatalf("layout paths = %v, want %v", gotPaths, wantPaths)
	}
	if first.Workspace != wantPaths[4] || first.Home != wantPaths[1] || first.Services != wantPaths[3] || first.Cache != wantPaths[0] || first.Platform != wantPaths[2] {
		t.Fatalf("layout = %#v", first)
	}
	for _, path := range wantPaths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode/type = %s/%t", path, info.Mode().Perm(), info.IsDir())
		}
	}
	assertComponentMetadata(t, root, "workspace", guest.DurabilityDurable)
	assertComponentMetadata(t, root, "home", guest.DurabilityDurable)
	assertComponentMetadata(t, root, "services", guest.DurabilityDurable)
	assertComponentMetadata(t, root, "cache", guest.DurabilityDisposable)
	identityBytes, err := os.ReadFile(filepath.Join(root, "platform", "volume-identity.json"))
	if err != nil {
		t.Fatalf("read volume identity: %v", err)
	}
	var identity guest.PersistentVolumeIdentity
	if err := json.Unmarshal(identityBytes, &identity); err != nil {
		t.Fatalf("decode volume identity: %v", err)
	}
	if identity.DeviceID != request.ExpectedDeviceID || identity.VolumeID != request.ExpectedVolumeID {
		t.Fatalf("persisted volume identity = %#v", identity)
	}
}

func TestBootstrapPersistentStateRejectsUntrustedMountEvidence(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name   string
		mount  guest.PersistentMount
		mutate func(*guest.PersistentStateRequest)
	}{
		{name: "not mounted", mount: guest.PersistentMount{MountPoint: root, DeviceID: "device-1", VolumeID: "volume-1", Writable: true}},
		{name: "wrong mount point", mount: guest.PersistentMount{Mounted: true, MountPoint: filepath.Join(root, "other"), DeviceID: "device-1", VolumeID: "volume-1", Writable: true}},
		{name: "wrong device", mount: guest.PersistentMount{Mounted: true, MountPoint: root, DeviceID: "device-other", VolumeID: "volume-1", Writable: true}},
		{name: "wrong volume", mount: guest.PersistentMount{Mounted: true, MountPoint: root, DeviceID: "device-1", VolumeID: "volume-other", Writable: true}},
		{name: "read only", mount: guest.PersistentMount{Mounted: true, MountPoint: root, DeviceID: "device-1", VolumeID: "volume-1"}},
		{name: "traversal root", mount: persistentMount(root), mutate: func(request *guest.PersistentStateRequest) {
			request.Root = root + string(os.PathSeparator) + ".." + string(os.PathSeparator) + filepath.Base(root)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := persistentStateRequest(root)
			if test.mutate != nil {
				test.mutate(&request)
			}
			if _, err := guest.BootstrapPersistentState(t.Context(), request, staticMountInspector{mount: test.mount}); err == nil {
				t.Fatal("BootstrapPersistentState() accepted untrusted mount evidence")
			}
		})
	}
}

func TestBootstrapPersistentStateRejectsSymlinkUnsafeLayoutAndIdentityDrift(t *testing.T) {
	t.Run("symlink component", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Symlink(t.TempDir(), filepath.Join(root, "workspace")); err != nil {
			t.Fatalf("create symlink: %v", err)
		}
		if _, err := guest.BootstrapPersistentState(t.Context(), persistentStateRequest(root), staticMountInspector{mount: persistentMount(root)}); err == nil {
			t.Fatal("BootstrapPersistentState() accepted symlink component")
		}
	})
	t.Run("unsafe permissions", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "workspace"), 0o755); err != nil {
			t.Fatalf("create workspace: %v", err)
		}
		if err := os.Chmod(filepath.Join(root, "workspace"), 0o755); err != nil {
			t.Fatalf("chmod workspace: %v", err)
		}
		if _, err := guest.BootstrapPersistentState(t.Context(), persistentStateRequest(root), staticMountInspector{mount: persistentMount(root)}); err == nil {
			t.Fatal("BootstrapPersistentState() accepted unsafe permissions")
		}
	})
	t.Run("unexpected root entry", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "unexpected"), []byte("state"), 0o600); err != nil {
			t.Fatalf("create unexpected entry: %v", err)
		}
		if _, err := guest.BootstrapPersistentState(t.Context(), persistentStateRequest(root), staticMountInspector{mount: persistentMount(root)}); err == nil {
			t.Fatal("BootstrapPersistentState() accepted unexpected root entry")
		}
	})
	t.Run("persisted identity drift", func(t *testing.T) {
		root := t.TempDir()
		request := persistentStateRequest(root)
		if _, err := guest.BootstrapPersistentState(t.Context(), request, staticMountInspector{mount: persistentMount(root)}); err != nil {
			t.Fatalf("initial bootstrap: %v", err)
		}
		drifted := persistentMount(root)
		drifted.DeviceID = "device-reused"
		request.ExpectedDeviceID = drifted.DeviceID
		if _, err := guest.BootstrapPersistentState(t.Context(), request, staticMountInspector{mount: drifted}); err == nil {
			t.Fatal("BootstrapPersistentState() accepted persisted identity drift")
		}
	})
}

func persistentStateRequest(root string) guest.PersistentStateRequest {
	return guest.PersistentStateRequest{Root: root, ExpectedDeviceID: "device-1", ExpectedVolumeID: "volume-1"}
}

func persistentMount(root string) guest.PersistentMount {
	return guest.PersistentMount{Mounted: true, MountPoint: root, DeviceID: "device-1", VolumeID: "volume-1", Writable: true}
}

func assertComponentMetadata(t *testing.T, root, name string, durability guest.DurabilityClass) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, "platform", "components", name+".json"))
	if err != nil {
		t.Fatalf("read %s metadata: %v", name, err)
	}
	var metadata guest.StateComponentMetadata
	if err := json.Unmarshal(content, &metadata); err != nil {
		t.Fatalf("decode %s metadata: %v", name, err)
	}
	if metadata.SchemaVersion != 1 || metadata.Name != name || metadata.Durability != durability {
		t.Fatalf("%s metadata = %#v", name, metadata)
	}
	if info, err := os.Stat(filepath.Join(root, "platform", "components", name+".json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%s metadata mode/error = %v/%v", name, info, err)
	}
}
