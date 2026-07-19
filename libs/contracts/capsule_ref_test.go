package contracts_test

import (
	"testing"

	"github.com/ahmedhesham6/sshai/libs/contracts"
)

func TestParseOwnedCapsuleRefSupportsNamedTagsAndExistingDigestRefs(t *testing.T) {
	tagged, err := contracts.ParseOwnedCapsuleRef("owner/user-1/agents:stable")
	if err != nil {
		t.Fatal(err)
	}
	if tagged.OwnerID != "user-1" || tagged.Name != "agents" || tagged.Tag != "stable" || tagged.Digest != "" {
		t.Fatalf("tagged ref = %#v", tagged)
	}
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pinned, err := contracts.ParseOwnedCapsuleRef(contracts.FormatOwnedCapsuleRef("user-1", digest))
	if err != nil {
		t.Fatal(err)
	}
	if pinned.OwnerID != "user-1" || pinned.Name != "capsule" || pinned.Digest != digest || pinned.Tag != "" {
		t.Fatalf("pinned ref = %#v", pinned)
	}
}
