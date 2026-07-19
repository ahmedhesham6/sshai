package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"

	guestcontrol "github.com/ahmedhesham6/sshai/apps/guest/control"
	"github.com/ahmedhesham6/sshai/apps/workflows"
	capsuleoci "github.com/ahmedhesham6/sshai/libs/capsule/oci"
)

type capsuleMaterializationGrantSource struct {
	provider capsuleoci.GrantProvider
}

func (source capsuleMaterializationGrantSource) MaterializationReadGrants(ctx context.Context, ownerUserID string, state workflows.EnvironmentCapsuleState) (map[string]guestcontrol.ReadGrant, error) {
	if source.provider == nil {
		return nil, permanentGuestTransportError{err: errors.New("mint Capsule materialization grants: provider is not configured")}
	}
	digests := materializationCapsuleDigests(state)
	keys, err := capsuleoci.CapsuleReadKeys(ctx, ownerUserID, digests, source.provider)
	if err != nil {
		return nil, err
	}
	result := make(map[string]guestcontrol.ReadGrant, len(keys))
	for _, key := range keys {
		grant, err := source.provider.Grant(ctx, capsuleoci.GrantRequest{OwnerID: ownerUserID, Key: key, Operation: capsuleoci.GrantRead})
		if err != nil {
			return nil, fmt.Errorf("mint Capsule materialization read grant: %w", err)
		}
		parsed, parseErr := url.Parse(grant.URL)
		if parseErr != nil || parsed.Scheme != "https" || parsed.Host == "" || grant.ExpiresAt.IsZero() {
			return nil, permanentGuestTransportError{err: errors.New("mint Capsule materialization read grant: provider returned an invalid serializable capability")}
		}
		result[key] = guestcontrol.ReadGrant{URL: grant.URL, ExpiresAt: grant.ExpiresAt}
	}
	return result, nil
}

func materializationCapsuleDigests(state workflows.EnvironmentCapsuleState) []string {
	seen := make(map[string]struct{})
	for _, component := range state.CapsuleLock.ResolvedComponents {
		seen[component.CapsuleDigest] = struct{}{}
		for _, source := range component.Sources {
			seen[source.CapsuleDigest] = struct{}{}
		}
	}
	if len(seen) == 0 && state.CapsuleLock.ProjectCapsuleDigest != "" {
		seen[state.CapsuleLock.ProjectCapsuleDigest] = struct{}{}
	}
	result := make([]string, 0, len(seen))
	for digest := range seen {
		result = append(result, digest)
	}
	sort.Strings(result)
	return result
}
