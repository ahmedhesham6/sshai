package controlplane

import (
	"testing"

	"github.com/ahmedhesham6/sshai/libs/contracts"
)

func TestValidatePublishProfileVersionRequestRejectsEmptyCapsuleRefs(t *testing.T) {
	body := validPublishProfileVersionRequest()
	body.CapsuleRefs = nil

	if err := ValidatePublishProfileVersionRequest(body); err == nil {
		t.Fatal("ValidatePublishProfileVersionRequest() accepted an empty capsuleRefs list")
	}
}

func TestValidatePublishProfileVersionRequestRejectsBadFreshnessPolicy(t *testing.T) {
	body := validPublishProfileVersionRequest()
	body.CapsuleRefs[0].FreshnessPolicy = "unsupported"

	if err := ValidatePublishProfileVersionRequest(body); err == nil {
		t.Fatal("ValidatePublishProfileVersionRequest() accepted an unsupported freshnessPolicy")
	}
}

func TestValidatePublishProfileVersionRequestRejectsMalformedRef(t *testing.T) {
	body := validPublishProfileVersionRequest()
	body.CapsuleRefs[0].Ref = "not a registry reference"

	if err := ValidatePublishProfileVersionRequest(body); err == nil {
		t.Fatal("ValidatePublishProfileVersionRequest() accepted a malformed Capsule Ref")
	}
}

func TestValidatePublishProfileVersionRequestAcceptsCanonicalOwnedDigestRefs(t *testing.T) {
	for _, ref := range []string{
		"owner/user-1/capsule@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"owner/user-2/capsule@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	} {
		body := validPublishProfileVersionRequest()
		body.CapsuleRefs[0].Ref = ref
		if err := ValidatePublishProfileVersionRequest(body); err != nil {
			t.Fatalf("ValidatePublishProfileVersionRequest(%q) = %v", ref, err)
		}
	}
}

func TestValidateCapsuleAccessRequestRejectsUnknownIntent(t *testing.T) {
	body := validCapsuleAccessRequest()
	body.Intent = "archive"

	if err := ValidateCapsuleAccessRequest(body); err == nil {
		t.Fatal("ValidateCapsuleAccessRequest() accepted an unknown intent")
	}
}

func validPublishProfileVersionRequest() contracts.PublishProfileVersionJSONRequestBody {
	return contracts.PublishProfileVersionJSONRequestBody{
		CapsuleRefs: []contracts.CapsuleRef{{
			Ref:             "owner/user-1/capsule@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			FreshnessPolicy: contracts.Track,
		}},
	}
}

func validCapsuleAccessRequest() contracts.CreateCapsuleAccessJSONRequestBody {
	return contracts.CreateCapsuleAccessJSONRequestBody{Intent: contracts.Pull}
}
