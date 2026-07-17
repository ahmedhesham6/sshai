package workflows_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/apps/workflows"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/testfixtures"
	"github.com/restatedev/sdk-go/ingress"
)

func TestProfileResolveFreshnessPoliciesDriveResolverAndApprovalClassification(t *testing.T) {
	tests := []struct {
		name             string
		freshness        domain.FreshnessPolicy
		upgrade          domain.UpgradePolicy
		approved         map[string]string
		wantApplyable    bool
		wantReview       bool
		wantDiff         string
		wantReason       string
		wantResolverCall bool
	}{
		{name: "track follows latest tag into safe path", freshness: domain.FreshnessTrack, upgrade: domain.UpgradeAutoSafe, wantApplyable: true, wantResolverCall: true},
		{name: "review reports diff since approval", freshness: domain.FreshnessReview, upgrade: domain.UpgradeManual, approved: map[string]string{"owner/user-1/capsule:stable": sha256Digest('a')}, wantReview: true, wantDiff: "changed editor", wantReason: "manual_upgrade", wantResolverCall: true},
		{name: "notify resolves clean candidate without applying", freshness: domain.FreshnessTrack, upgrade: domain.UpgradeNotify, wantReason: "notify_upgrade", wantResolverCall: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
			version := profileVersionFixture(t, now)
			version.CapsuleRefs[0] = domain.CapsuleRef{Ref: "owner/user-1/capsule:stable", FreshnessPolicy: test.freshness}
			actions := &profileResolveActionsFake{version: version}
			resolver := &policyResolverFake{resolution: workflows.CapsuleResolution{
				OwnerID: "user-1", Digest: sha256Digest('b'), DiffSinceLastApproval: test.wantDiff,
				Components: []domain.Component{{ID: "config:editor", Type: domain.ComponentConfig, MediaType: "application/json", Digest: sha256Digest('c'), Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative}},
			}}
			environment := testfixtures.StartRestate(t, workflows.ProfileResolveDefinition(actions, resolver, &resolveIDs{values: []string{"lock-policy"}}, func() time.Time { return now }))
			input := workflows.ProfileResolveInput{
				OperationID: "operation-" + strings.ReplaceAll(test.name, " ", "-"), EnvironmentID: "environment-1",
				ProfileVersionID: "version-1", OwnerID: "user-1", UpgradePolicy: test.upgrade,
				LastApprovedCapsuleDigests: test.approved,
			}
			if err := workflows.NewProfileResolveClient(environment.Ingress()).SendProfileResolve(t.Context(), input); err != nil {
				t.Fatalf("submit Profile resolve: %v", err)
			}
			output, err := ingress.WorkflowHandle[workflows.ProfileResolveOutput](environment.Ingress(), workflows.ProfileResolveService, input.OperationID).Attach(t.Context())
			if err != nil {
				t.Fatalf("await Profile resolve: %v", err)
			}
			if output.Applyable != test.wantApplyable || output.RequiresReview != test.wantReview || output.UpgradeReason != test.wantReason || !strings.Contains(output.DiffSinceLastApproval, test.wantDiff) {
				t.Fatalf("Profile resolve output = %#v, want applyable=%v review=%v reason=%q diff=%q", output, test.wantApplyable, test.wantReview, test.wantReason, test.wantDiff)
			}
			if resolver.calls != 1 || !test.wantResolverCall || resolver.lastRef.FreshnessPolicy != test.freshness {
				t.Fatalf("resolver calls/ref = %d/%#v, want one %q", resolver.calls, resolver.lastRef, test.freshness)
			}
		})
	}
}

func TestProfileResolvePinRequiresAnExactDigestReference(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	version := profileVersionFixture(t, now)
	version.CapsuleRefs[0] = domain.CapsuleRef{Ref: "owner/user-1/capsule:stable", FreshnessPolicy: domain.FreshnessPin}
	actions := &profileResolveActionsFake{version: version}
	resolver := &policyResolverFake{resolution: workflows.CapsuleResolution{OwnerID: "user-1", Digest: sha256Digest('a')}}
	environment := testfixtures.StartRestate(t, workflows.ProfileResolveDefinition(actions, resolver, &resolveIDs{values: []string{"lock-pin"}}, func() time.Time { return now }))
	input := workflows.ProfileResolveInput{OperationID: "operation-pin", EnvironmentID: "environment-1", ProfileVersionID: "version-1", OwnerID: "user-1", UpgradePolicy: domain.UpgradeAutoSafe}
	if err := workflows.NewProfileResolveClient(environment.Ingress()).SendProfileResolve(t.Context(), input); err != nil {
		t.Fatalf("submit pin Profile resolve: %v", err)
	}
	if _, err := ingress.WorkflowHandle[workflows.ProfileResolveOutput](environment.Ingress(), workflows.ProfileResolveService, input.OperationID).Attach(t.Context()); err == nil {
		t.Fatal("tag-based pin Profile resolve succeeded, want exact digest rejection")
	}
	if actions.persistCalls != 0 {
		t.Fatalf("pin rejection persisted %d locks", actions.persistCalls)
	}
}

func TestProfileResolveAutoSafeVetoKeepsCandidateNonApplyable(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	version := profileVersionFixture(t, now)
	actions := &profileResolveActionsFake{version: version}
	resolver := &policyResolverFake{resolution: workflows.CapsuleResolution{
		OwnerID: "user-1", Digest: sha256Digest('a'),
		Components: []domain.Component{{ID: "config:editor", Type: domain.ComponentConfig, MediaType: "application/json", Digest: sha256Digest('c'), Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative}},
	}}
	environment := testfixtures.StartRestate(t, workflows.ProfileResolveDefinition(actions, resolver, &resolveIDs{values: []string{"lock-veto"}}, func() time.Time { return now }))
	input := workflows.ProfileResolveInput{
		OperationID: "operation-veto", EnvironmentID: "environment-1", ProfileVersionID: "version-1", OwnerID: "user-1",
		UpgradePolicy: domain.UpgradeAutoSafe,
		AutoSafePlan:  &domain.AutoSafePlan{ManagedTargetsClean: true, IntegrationChanged: true},
	}
	if err := workflows.NewProfileResolveClient(environment.Ingress()).SendProfileResolve(t.Context(), input); err != nil {
		t.Fatalf("submit vetoed Profile resolve: %v", err)
	}
	output, err := ingress.WorkflowHandle[workflows.ProfileResolveOutput](environment.Ingress(), workflows.ProfileResolveService, input.OperationID).Attach(t.Context())
	if err != nil {
		t.Fatalf("await vetoed Profile resolve: %v", err)
	}
	if output.Applyable || !output.RequiresReview || output.UpgradeReason != "integration_changed" {
		t.Fatalf("vetoed Profile resolve output = %#v, want explicit non-applyable review", output)
	}
}

func TestProfileResolveIgnoresCallerManagedTargetsCleanWhenPersistedStateIsDirty(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	version := profileVersionFixture(t, now)
	version.CapsuleRefs[0] = domain.CapsuleRef{Ref: "owner/user-1/capsule:stable", FreshnessPolicy: domain.FreshnessTrack}
	actions := &profileResolveActionsFake{version: version, state: workflows.ProfileResolveState{
		ManagedTargets: []domain.ManagedTargetState{{
			ComponentID: "config:editor", DesiredDigest: sha256Digest('c'), LastAppliedDigest: sha256Digest('a'), ObservedDigest: sha256Digest('b'),
		}},
	}}
	resolver := &policyResolverFake{resolution: workflows.CapsuleResolution{
		OwnerID: "user-1", Digest: sha256Digest('c'),
		Components: []domain.Component{{ID: "config:editor", Type: domain.ComponentConfig, MediaType: "application/json", Digest: sha256Digest('d'), Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative}},
	}}
	environment := testfixtures.StartRestate(t, workflows.ProfileResolveDefinition(actions, resolver, &resolveIDs{values: []string{"lock-dirty"}}, func() time.Time { return now }))
	input := workflows.ProfileResolveInput{
		OperationID: "operation-dirty", EnvironmentID: "environment-1", ProfileVersionID: "version-1", OwnerID: "user-1",
		UpgradePolicy: domain.UpgradeAutoSafe,
		AutoSafePlan:  &domain.AutoSafePlan{ManagedTargetsClean: true},
	}
	if err := workflows.NewProfileResolveClient(environment.Ingress()).SendProfileResolve(t.Context(), input); err != nil {
		t.Fatalf("submit dirty-target Profile resolve: %v", err)
	}
	output, err := ingress.WorkflowHandle[workflows.ProfileResolveOutput](environment.Ingress(), workflows.ProfileResolveService, input.OperationID).Attach(t.Context())
	if err != nil {
		t.Fatalf("await dirty-target Profile resolve: %v", err)
	}
	if output.Applyable || !output.RequiresReview || output.UpgradeReason != "managed_targets_not_clean" {
		t.Fatalf("caller-clean dirty-target output = %#v, want closed non-applyable decision", output)
	}
}

func TestProfileResolvePersistedManualPolicyCannotBeWidenedByCaller(t *testing.T) {
	for _, test := range []struct {
		name   string
		policy domain.UpgradePolicy
	}{
		{name: "omitted policy", policy: ""},
		{name: "auto safe policy", policy: domain.UpgradeAutoSafe},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
			persistedPolicy := domain.UpgradeManual
			actions := &profileResolveActionsFake{
				version: profileVersionFixture(t, now),
				state: workflows.ProfileResolveState{
					PersistedUpgradePolicy: &persistedPolicy,
				},
			}
			resolver := &policyResolverFake{resolution: workflows.CapsuleResolution{
				OwnerID: "user-1", Digest: sha256Digest('a'),
				Components: []domain.Component{{
					ID: "config:editor", Type: domain.ComponentConfig, MediaType: "application/json",
					Digest: sha256Digest('c'), Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative,
				}},
			}}
			environment := testfixtures.StartRestate(t, workflows.ProfileResolveDefinition(
				actions, resolver, &resolveIDs{values: []string{"lock-persisted-policy-" + strings.ReplaceAll(test.name, " ", "-")}}, func() time.Time { return now },
			))
			input := workflows.ProfileResolveInput{
				OperationID:   "operation-persisted-policy-" + strings.ReplaceAll(test.name, " ", "-"),
				EnvironmentID: "environment-1", ProfileVersionID: "version-1", OwnerID: "user-1",
				UpgradePolicy: test.policy,
			}
			if err := workflows.NewProfileResolveClient(environment.Ingress()).SendProfileResolve(t.Context(), input); err != nil {
				t.Fatalf("submit persisted-policy Profile resolve: %v", err)
			}
			output, err := ingress.WorkflowHandle[workflows.ProfileResolveOutput](environment.Ingress(), workflows.ProfileResolveService, input.OperationID).Attach(t.Context())
			if err != nil {
				t.Fatalf("await persisted-policy Profile resolve: %v", err)
			}
			if output.Applyable || !output.RequiresReview || output.UpgradeReason != "manual_upgrade" {
				t.Fatalf("persisted manual policy output = %#v, want non-applyable manual review", output)
			}
		})
	}
}

func TestProfileResolveReviewCandidateEchoCannotAuthorizeWithoutPersistedApproval(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	version := profileVersionFixture(t, now)
	version.CapsuleRefs[0] = domain.CapsuleRef{Ref: "owner/user-1/capsule:stable", FreshnessPolicy: domain.FreshnessReview}
	candidate := sha256Digest('c')
	actions := &profileResolveActionsFake{version: version}
	resolver := &policyResolverFake{resolution: workflows.CapsuleResolution{
		OwnerID: "user-1", Digest: candidate,
		Components: []domain.Component{{ID: "config:editor", Type: domain.ComponentConfig, MediaType: "application/json", Digest: sha256Digest('d'), Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative}},
	}}
	environment := testfixtures.StartRestate(t, workflows.ProfileResolveDefinition(actions, resolver, &resolveIDs{values: []string{"lock-approval"}}, func() time.Time { return now }))
	input := workflows.ProfileResolveInput{
		OperationID: "operation-approval", EnvironmentID: "environment-1", ProfileVersionID: "version-1", OwnerID: "user-1",
		UpgradePolicy:              domain.UpgradeAutoSafe,
		LastApprovedCapsuleDigests: map[string]string{version.CapsuleRefs[0].Ref: candidate},
	}
	if err := workflows.NewProfileResolveClient(environment.Ingress()).SendProfileResolve(t.Context(), input); err != nil {
		t.Fatalf("submit forged-approval Profile resolve: %v", err)
	}
	output, err := ingress.WorkflowHandle[workflows.ProfileResolveOutput](environment.Ingress(), workflows.ProfileResolveService, input.OperationID).Attach(t.Context())
	if err != nil {
		t.Fatalf("await forged-approval Profile resolve: %v", err)
	}
	if output.Applyable || !output.RequiresReview || output.UpgradeReason != "freshness_review_required" {
		t.Fatalf("caller-approved candidate output = %#v, want persisted-approval review", output)
	}
}

func TestProfileResolveComposesOrderedProfileCapsulesThenEnvironmentProjectCapsule(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	version := profileVersionFixture(t, now)
	profileDigest := sha256Digest('a')
	projectDigest := sha256Digest('f')
	version.CapsuleRefs[0] = domain.CapsuleRef{Ref: "owner/user-1/capsule@" + profileDigest, FreshnessPolicy: domain.FreshnessPin, Exclusions: []string{"config:excluded"}}
	actions := &profileResolveActionsFake{version: version}
	resolver := &multiPolicyResolverFake{resolutions: map[string]workflows.CapsuleResolution{
		version.CapsuleRefs[0].Ref: {OwnerID: "user-1", Digest: profileDigest, Components: []domain.Component{
			{ID: "config:profile", Type: domain.ComponentConfig, MediaType: "application/json", Digest: sha256Digest('c'), Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative},
			{ID: "config:excluded", Type: domain.ComponentConfig, MediaType: "application/json", Digest: sha256Digest('d'), Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative},
		}},
		"owner/user-1/capsule@" + projectDigest: {OwnerID: "user-1", Digest: projectDigest, Components: []domain.Component{
			{ID: "config:project", Type: domain.ComponentConfig, MediaType: "application/json", Digest: sha256Digest('e'), Scope: domain.ScopeProject, TrustClass: domain.TrustDeclarative},
		}},
	}}
	environment := testfixtures.StartRestate(t, workflows.ProfileResolveDefinition(actions, resolver, &resolveIDs{values: []string{"lock-project"}}, func() time.Time { return now }))
	project := projectDigest
	input := workflows.ProfileResolveInput{
		OperationID: "operation-project", EnvironmentID: "environment-1", ProfileVersionID: "version-1",
		OwnerID: "user-1", ProjectCapsuleDigest: &project, UpgradePolicy: domain.UpgradeAutoSafe,
	}
	if err := workflows.NewProfileResolveClient(environment.Ingress()).SendProfileResolve(t.Context(), input); err != nil {
		t.Fatalf("submit project Profile resolve: %v", err)
	}
	if _, err := ingress.WorkflowHandle[workflows.ProfileResolveOutput](environment.Ingress(), workflows.ProfileResolveService, input.OperationID).Attach(t.Context()); err != nil {
		t.Fatalf("await project Profile resolve: %v", err)
	}
	lock := actions.lock.Snapshot()
	if lock.ProjectCapsuleDigest != projectDigest || len(lock.Capsules) != 1 {
		t.Fatalf("lock capsule composition = %#v, want profile lock plus project digest", lock)
	}
	if _, found := lock.ResolvedComponents["config:excluded"]; found {
		t.Fatal("workflow resolved an excluded Component")
	}
	if lock.ResolvedComponents["config:project"].CapsuleDigest != projectDigest || lock.ResolvedComponents["config:project"].Scope != domain.ScopeProject {
		t.Fatalf("project Component lock entry = %#v, want project Capsule ownership", lock.ResolvedComponents["config:project"])
	}
}

func TestProfileResolveEmitsLockResolutionMetricThroughNarrowSeam(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	actions := &profileResolveActionsFake{version: profileVersionFixture(t, now)}
	resolver := &policyResolverFake{resolution: workflows.CapsuleResolution{OwnerID: "user-1", Digest: sha256Digest('a')}}
	metrics := &counterMetricsFake{counts: make(map[string]int64)}
	environment := testfixtures.StartRestate(t, workflows.ProfileResolveDefinition(actions, resolver, &resolveIDs{values: []string{"lock-metric"}}, func() time.Time { return now }, metrics))
	input := workflows.ProfileResolveInput{OperationID: "operation-metric", EnvironmentID: "environment-1", ProfileVersionID: "version-1", OwnerID: "user-1"}
	if err := workflows.NewProfileResolveClient(environment.Ingress()).SendProfileResolve(t.Context(), input); err != nil {
		t.Fatalf("submit metric Profile resolve: %v", err)
	}
	if _, err := ingress.WorkflowHandle[workflows.ProfileResolveOutput](environment.Ingress(), workflows.ProfileResolveService, input.OperationID).Attach(t.Context()); err != nil {
		t.Fatalf("await metric Profile resolve: %v", err)
	}
	if metrics.counts[domain.MetricLockResolutionsTotal] != 1 {
		t.Fatalf("lock resolution metrics = %#v, want one resolution", metrics.counts)
	}
}

type policyResolverFake struct {
	resolution workflows.CapsuleResolution
	lastRef    domain.CapsuleRef
	calls      int
}

type multiPolicyResolverFake struct {
	resolutions map[string]workflows.CapsuleResolution
	calls       int
}

type counterMetricsFake struct {
	counts map[string]int64
}

func (metrics *counterMetricsFake) AddCounter(name string, value int64) {
	metrics.counts[name] += value
}

func (resolver *multiPolicyResolverFake) Resolve(_ context.Context, _ string, ref domain.CapsuleRef) (workflows.CapsuleResolution, error) {
	resolver.calls++
	resolution, ok := resolver.resolutions[ref.Ref]
	if !ok {
		return workflows.CapsuleResolution{}, context.Canceled
	}
	return resolution, nil
}

func (resolver *policyResolverFake) Resolve(_ context.Context, _ string, ref domain.CapsuleRef) (workflows.CapsuleResolution, error) {
	resolver.calls++
	resolver.lastRef = ref
	return resolver.resolution, nil
}

func sha256Digest(character byte) string {
	return "sha256:" + strings.Repeat(string(character), 64)
}
