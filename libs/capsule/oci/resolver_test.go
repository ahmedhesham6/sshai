package oci_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	oci "github.com/ahmedhesham6/sshai/libs/capsule/oci"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

type stubGrantProvider struct{}

func (stubGrantProvider) Grant(context.Context, oci.GrantRequest) (oci.Grant, error) {
	return oci.Grant{}, errors.New("stub grant provider: unused")
}

func TestResolverResolveRejectsUnconfiguredResolver(t *testing.T) {
	resolver := oci.NewResolver(nil)
	ref := domain.CapsuleRef{Ref: "owner/owner-1/capsule@sha256:" + strings.Repeat("a", 64), FreshnessPolicy: domain.FreshnessPin}
	if _, err := resolver.Resolve(t.Context(), "owner-1", ref); err == nil {
		t.Fatal("Resolve() error = nil, want configuration error")
	}
}

func TestResolverResolveRejectsMalformedRef(t *testing.T) {
	resolver := oci.NewResolver(stubGrantProvider{})
	ref := domain.CapsuleRef{Ref: "not-a-capsule-ref", FreshnessPolicy: domain.FreshnessPin}
	if _, err := resolver.Resolve(t.Context(), "owner-1", ref); err == nil {
		t.Fatal("Resolve() error = nil, want ref parse error")
	}
}

func TestResolverResolveRejectsForeignOwnerRef(t *testing.T) {
	resolver := oci.NewResolver(stubGrantProvider{})
	ref := domain.CapsuleRef{Ref: "owner/owner-2/capsule@sha256:" + strings.Repeat("a", 64), FreshnessPolicy: domain.FreshnessPin}
	_, err := resolver.Resolve(t.Context(), "owner-1", ref)
	if err == nil || !strings.Contains(err.Error(), "does not match requesting owner") {
		t.Fatalf("Resolve() error = %v, want owner mismatch error", err)
	}
}

// Capsule Refs may use a moving tag form (owner/<owner>/capsule:<tag>), which
// domain.FreshnessTrack and domain.FreshnessReview both rely on. The MVP
// capsule store is content-addressed only (ADR 0009): it has no tag/name to
// digest index and no write path that would populate one, so Resolve must
// reject tag refs with a clear error instead of fabricating a resolution.
func TestResolverResolveRejectsTagRef(t *testing.T) {
	resolver := oci.NewResolver(stubGrantProvider{})
	ref := domain.CapsuleRef{Ref: "owner/owner-1/capsule:stable", FreshnessPolicy: domain.FreshnessTrack}
	_, err := resolver.Resolve(t.Context(), "owner-1", ref)
	if err == nil || !strings.Contains(err.Error(), "tag") {
		t.Fatalf("Resolve() error = %v, want tag-unsupported error", err)
	}
}
