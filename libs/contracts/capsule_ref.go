package contracts

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var ownedCapsuleRefPattern = regexp.MustCompile(`^owner/([A-Za-z0-9][A-Za-z0-9._-]{0,127})/capsule(:([A-Za-z0-9][A-Za-z0-9._-]{0,127})|@(sha256:[a-f0-9]{64}))$`)

// OwnedCapsuleRef is the owner-scoped Capsule reference used by the OCI object
// keys. It may identify a tag or an exact content digest.
type OwnedCapsuleRef struct {
	OwnerID string
	Digest  string
	Tag     string
}

// ParseOwnedCapsuleRef parses the canonical self-owned Capsule store ref.
func ParseOwnedCapsuleRef(ref string) (OwnedCapsuleRef, error) {
	if strings.TrimSpace(ref) != ref {
		return OwnedCapsuleRef{}, errors.New("Capsule Ref must not contain surrounding whitespace")
	}
	matches := ownedCapsuleRefPattern.FindStringSubmatch(ref)
	if len(matches) != 5 {
		return OwnedCapsuleRef{}, fmt.Errorf("Capsule Ref %q is not a canonical owner-scoped tag or digest reference", ref)
	}
	return OwnedCapsuleRef{OwnerID: matches[1], Tag: matches[3], Digest: matches[4]}, nil
}

// FormatOwnedCapsuleRef constructs the canonical MVP Capsule store ref.
func FormatOwnedCapsuleRef(ownerID, digest string) string {
	return "owner/" + ownerID + "/capsule@" + digest
}
