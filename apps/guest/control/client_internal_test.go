package control

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/ahmedhesham6/sshai/apps/guest"
)

func TestStreamingProjectSeedEncodingPreservesJSONContract(t *testing.T) {
	request := ProjectSeedRequest{
		Target: Target{OwnerUserID: "user-1", EnvironmentID: "environment-1", RuntimeID: "runtime-1", ProviderID: "instance-1", PrivateIPv4: "10.0.0.8"},
		Seed: guest.ProjectSeedApplicationInput{
			RepositoryURL: "https://example.invalid/project.git", BaseRevision: "0123456789012345678901234567890123456789",
			GitBundle:    guest.ProjectSeedArtifact{SHA256: "sha256:bundle", Content: []byte{0, 1, 2}},
			TrackedPatch: guest.ProjectSeedArtifact{SHA256: "sha256:patch", Content: []byte{}},
			UntrackedTar: guest.ProjectSeedArtifact{SHA256: "sha256:tar", Content: nil},
			Manifest:     guest.ProjectSeedArtifact{SHA256: "sha256:manifest", Content: []byte(`[{"path":"README.md"}]`)},
		},
	}
	want, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := encodeProjectSeedRequest(&got, request); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("streamed Project Seed JSON = %s, want %s", got.Bytes(), want)
	}
}
