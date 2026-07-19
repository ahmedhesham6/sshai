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

func TestResolverResolveRejectsTagRefWithoutIndex(t *testing.T) {
	resolver := oci.NewResolver(stubGrantProvider{})
	ref := domain.CapsuleRef{Ref: "owner/owner-1/capsule:stable", FreshnessPolicy: domain.FreshnessTrack}
	_, err := resolver.Resolve(t.Context(), "owner-1", ref)
	if err == nil || !strings.Contains(err.Error(), "tag index") {
		t.Fatalf("Resolve() error = %v, want tag-index configuration error", err)
	}
}

func TestResolverResolveUsesOwnerNameAndTagIndex(t *testing.T) {
	provider := newFileGrantProvider(t)
	client, err := oci.NewClient("owner-1", provider)
	if err != nil {
		t.Fatal(err)
	}
	value := buildTestCapsule(t, map[string]string{"config:editor": "editor = vim\n"})
	if _, err := client.Publish(t.Context(), value); err != nil {
		t.Fatal(err)
	}
	tags := &tagIndexFake{digest: value.Digest}
	resolver := oci.NewResolverWithTagIndex(provider, tags)
	ref := domain.CapsuleRef{Ref: "owner/owner-1/agents:stable", FreshnessPolicy: domain.FreshnessTrack}
	resolution, err := resolver.Resolve(t.Context(), "owner-1", ref)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if tags.ownerID != "owner-1" || tags.name != "agents" || tags.tag != "stable" || resolution.Digest != value.Digest || len(resolution.Components) != 1 {
		t.Fatalf("tag lookup/resolution = fake:%#v resolution:%#v", tags, resolution)
	}
}

type tagIndexFake struct {
	ownerID string
	name    string
	tag     string
	digest  string
}

func (index *tagIndexFake) ResolveCapsuleTag(_ context.Context, ownerID, name, tag string) (string, error) {
	index.ownerID, index.name, index.tag = ownerID, name, tag
	return index.digest, nil
}
