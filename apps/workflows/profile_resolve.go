package workflows

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/domain"
	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"
)

const ProfileResolveService = "ProfileResolve"

// ProfileResolveInput is the durable input for one profile.resolve Operation.
// OperationID is the Restate workflow key and is also the projected Operation
// identity. ProjectCapsuleDigest is optional for environments without a
// reviewed project Capsule yet.
type ProfileResolveInput struct {
	OperationID          string               `json:"operationId"`
	EnvironmentID        string               `json:"environmentId"`
	ProfileVersionID     string               `json:"profileVersionId"`
	OwnerID              string               `json:"ownerId"`
	ProjectCapsuleDigest *string              `json:"projectCapsuleDigest,omitempty"`
	UpgradePolicy        domain.UpgradePolicy `json:"upgradePolicy,omitempty"`
	// These legacy fields remain decodable for replay compatibility. Security
	// decisions ignore them and use ProfileResolveState loaded from storage.
	LastApprovedCapsuleDigests map[string]string    `json:"lastApprovedCapsuleDigests,omitempty"`
	AutoSafePlan               *domain.AutoSafePlan `json:"autoSafePlan,omitempty"`
}

// ProfileResolveState is loaded from the authenticated Environment state
// boundary. It contains the only approval and managed-target inputs accepted
// by the safety decision.
type ProfileResolveState struct {
	ManagedTargets             []domain.ManagedTargetState
	LastApprovedCapsuleDigests map[string]string
	PersistedUpgradePolicy     *domain.UpgradePolicy
}

type ProfileResolveOutput struct {
	LockID                string                          `json:"lockId"`
	Digest                string                          `json:"digest"`
	Classification        domain.ResolutionClassification `json:"classification"`
	RequiresReview        bool                            `json:"requiresReview"`
	Applyable             bool                            `json:"applyable"`
	UpgradeReason         string                          `json:"upgradeReason,omitempty"`
	DiffSinceLastApproval string                          `json:"diffSinceLastApproval,omitempty"`
}

// CapsuleResolution is the content-addressed result for one Capsule Ref.
// Implementations may use a registry/S3 store. The workflow deliberately
// keeps that lookup behind this seam so tag resolution can be added without
// changing the lock or Restate contracts.
type CapsuleResolution struct {
	// OwnerID is the store namespace verified by the resolver. It is required
	// so a digest cannot be detached from the owner-scoped object lookup that
	// produced it.
	OwnerID               string
	Digest                string
	Components            []domain.Component
	DiffSinceLastApproval string
}

type CapsuleResolver interface {
	Resolve(context.Context, string, domain.CapsuleRef) (CapsuleResolution, error)
}

type CapsuleResolverFunc func(context.Context, string, domain.CapsuleRef) (CapsuleResolution, error)

func (fn CapsuleResolverFunc) Resolve(ctx context.Context, ownerID string, ref domain.CapsuleRef) (CapsuleResolution, error) {
	return fn(ctx, ownerID, ref)
}

type ProfileVersionData = domain.ProfileVersionData

type ProfileResolveActions interface {
	RecordProfileResolveInvocation(context.Context, string, string, string, string, *string, time.Time) error
	LoadProfileVersion(context.Context, string, string) (ProfileVersionData, error)
	LoadProfileResolveState(context.Context, string) (ProfileResolveState, error)
	PersistCapsuleLock(context.Context, domain.CapsuleLockSnapshot) (domain.CapsuleLockSnapshot, error)
	CompleteProfileResolve(context.Context, string, time.Time) error
}

type ProfileResolveRepository interface {
	RecordProfileResolveInvocation(context.Context, string, string, string, string, *string, time.Time) error
	LoadProfileVersion(context.Context, string, string) (ProfileVersionData, error)
	PersistCapsuleLock(context.Context, domain.CapsuleLock) (domain.CapsuleLock, error)
	CompleteProfileResolve(context.Context, string, time.Time) error
}

type profileResolveStateRepository interface {
	LoadProfileResolveState(context.Context, string) (ProfileResolveState, error)
}

type profileResolveActions struct {
	repository ProfileResolveRepository
}

func NewProfileResolveActions(repository ProfileResolveRepository) ProfileResolveActions {
	return &profileResolveActions{repository: repository}
}

func (actions *profileResolveActions) RecordProfileResolveInvocation(ctx context.Context, operationID, invocationID, environmentID, profileVersionID string, projectCapsuleDigest *string, at time.Time) error {
	return actions.repository.RecordProfileResolveInvocation(ctx, operationID, invocationID, environmentID, profileVersionID, projectCapsuleDigest, at)
}

func (actions *profileResolveActions) LoadProfileVersion(ctx context.Context, environmentID, profileVersionID string) (ProfileVersionData, error) {
	return actions.repository.LoadProfileVersion(ctx, environmentID, profileVersionID)
}

func (actions *profileResolveActions) LoadProfileResolveState(ctx context.Context, environmentID string) (ProfileResolveState, error) {
	reader, ok := any(actions.repository).(profileResolveStateRepository)
	if !ok {
		return ProfileResolveState{}, errors.New("Profile resolve state repository is not configured")
	}
	return reader.LoadProfileResolveState(ctx, environmentID)
}

func (actions *profileResolveActions) PersistCapsuleLock(ctx context.Context, snapshot domain.CapsuleLockSnapshot) (domain.CapsuleLockSnapshot, error) {
	lock, err := domain.CreateCapsuleLock(snapshot)
	if err != nil {
		return domain.CapsuleLockSnapshot{}, err
	}
	persisted, err := actions.repository.PersistCapsuleLock(ctx, lock)
	if err != nil {
		return domain.CapsuleLockSnapshot{}, err
	}
	return persisted.Snapshot(), nil
}

func (actions *profileResolveActions) CompleteProfileResolve(ctx context.Context, operationID string, at time.Time) error {
	return actions.repository.CompleteProfileResolve(ctx, operationID, at)
}

type profileResolveWorkflow struct {
	actions  ProfileResolveActions
	resolver CapsuleResolver
	ids      IDGenerator
	now      func() time.Time
	metrics  domain.Metrics
}

func ProfileResolveDefinition(actions ProfileResolveActions, resolver CapsuleResolver, ids IDGenerator, now func() time.Time, metrics ...domain.Metrics) restate.ServiceDefinition {
	var sink domain.Metrics
	if len(metrics) > 0 {
		sink = metrics[0]
	}
	workflow := &profileResolveWorkflow{actions: actions, resolver: resolver, ids: ids, now: now, metrics: sink}
	return restate.NewWorkflow(ProfileResolveService).Handler(
		RunHandler,
		restate.NewWorkflowHandler(workflow.Run),
	)
}

func (workflow *profileResolveWorkflow) Run(ctx restate.WorkflowContext, input ProfileResolveInput) (ProfileResolveOutput, error) {
	if restate.Key(ctx) != input.OperationID {
		return ProfileResolveOutput{}, restate.TerminalErrorf("workflow key does not match Operation ID")
	}
	if workflow.actions == nil || workflow.resolver == nil || workflow.ids == nil || workflow.now == nil {
		return ProfileResolveOutput{}, restate.TerminalErrorf("Profile resolve workflow dependencies are incomplete")
	}
	if strings.TrimSpace(input.OwnerID) == "" {
		return ProfileResolveOutput{}, restate.TerminalErrorf("Profile resolve workflow owner ID is required")
	}
	upgradePolicy := input.UpgradePolicy
	if upgradePolicy == "" {
		upgradePolicy = domain.UpgradeManual
	}
	if !upgradePolicy.Valid() {
		return ProfileResolveOutput{}, restate.TerminalErrorf("Profile resolve workflow upgrade policy %q is invalid", upgradePolicy)
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return workflow.actions.RecordProfileResolveInvocation(runCtx, input.OperationID, runCtx.Request().ID,
			input.EnvironmentID, input.ProfileVersionID, input.ProjectCapsuleDigest, workflow.now().UTC())
	}, restate.WithName("record-invocation")); err != nil {
		return ProfileResolveOutput{}, err
	}
	version, err := restate.Run(ctx, func(runCtx restate.RunContext) (ProfileVersionData, error) {
		return workflow.actions.LoadProfileVersion(runCtx, input.EnvironmentID, input.ProfileVersionID)
	}, restate.WithName("load-profile-version"))
	if err != nil {
		return ProfileResolveOutput{}, classifyDurableError(err)
	}
	state, err := restate.Run(ctx, func(runCtx restate.RunContext) (ProfileResolveState, error) {
		return workflow.actions.LoadProfileResolveState(runCtx, input.EnvironmentID)
	}, restate.WithName("load-profile-resolve-state"))
	if err != nil {
		return ProfileResolveOutput{}, classifyDurableError(err)
	}
	if state.PersistedUpgradePolicy != nil {
		if !state.PersistedUpgradePolicy.Valid() {
			return ProfileResolveOutput{}, restate.TerminalErrorf("persisted Environment upgrade policy %q is invalid", *state.PersistedUpgradePolicy)
		}
		upgradePolicy = narrowerUpgradePolicy(upgradePolicy, *state.PersistedUpgradePolicy)
	}
	resolved, err := restate.Run(ctx, func(runCtx restate.RunContext) (resolvedCapsuleSet, error) {
		return workflow.resolveCapsules(runCtx, input.OwnerID, version, input.ProjectCapsuleDigest, state.LastApprovedCapsuleDigests)
	}, restate.WithName("resolve-capsules"))
	if err != nil {
		return ProfileResolveOutput{}, classifyDurableError(err)
	}
	applyable, requiresReview, upgradeReason := classifyUpgrade(upgradePolicy, resolved, state.ManagedTargets, input.AutoSafePlan)
	if workflow.metrics != nil {
		workflow.metrics.AddCounter(domain.MetricLockResolutionsTotal, 1)
	}
	lock, err := restate.Run(ctx, func(runCtx restate.RunContext) (domain.CapsuleLockSnapshot, error) {
		if workflow.ids == nil || workflow.now == nil {
			return domain.CapsuleLockSnapshot{}, errors.New("Profile resolve workflow identity dependencies are incomplete")
		}
		created, err := domain.CreateCapsuleLock(domain.CapsuleLockSnapshot{
			ID: workflow.ids.NewID(), EnvironmentID: input.EnvironmentID, ProfileVersionID: input.ProfileVersionID,
			ProjectCapsuleDigest: resolved.ProjectDigest, Capsules: resolved.LockedCapsules,
			ResolvedComponents: resolved.Components, CreatedAt: workflow.now().UTC(),
		})
		if err != nil {
			return domain.CapsuleLockSnapshot{}, err
		}
		return created.Snapshot(), nil
	}, restate.WithName("create-lock"))
	if err != nil {
		return ProfileResolveOutput{}, restate.TerminalErrorf("create Capsule Lock: %v", err)
	}
	persisted, err := restate.Run(ctx, func(runCtx restate.RunContext) (domain.CapsuleLockSnapshot, error) {
		return workflow.actions.PersistCapsuleLock(runCtx, lock)
	}, restate.WithName("persist-lock"))
	if err != nil {
		return ProfileResolveOutput{}, classifyDurableError(err)
	}
	if err := restate.RunVoid(ctx, func(runCtx restate.RunContext) error {
		return workflow.actions.CompleteProfileResolve(runCtx, input.OperationID, workflow.now().UTC())
	}, restate.WithName("complete-projection")); err != nil {
		return ProfileResolveOutput{}, classifyDurableError(err)
	}
	return ProfileResolveOutput{
		LockID: persisted.ID, Digest: persisted.Digest, Classification: resolved.Classification,
		RequiresReview: requiresReview, Applyable: applyable, UpgradeReason: upgradeReason,
		DiffSinceLastApproval: formatApprovalDiffs(resolved.DiffSinceLastApproval),
	}, nil
}

func narrowerUpgradePolicy(first, second domain.UpgradePolicy) domain.UpgradePolicy {
	rank := func(policy domain.UpgradePolicy) int {
		switch policy {
		case domain.UpgradeManual:
			return 0
		case domain.UpgradeNotify:
			return 1
		default:
			return 2
		}
	}
	if rank(first) <= rank(second) {
		return first
	}
	return second
}

type resolvedCapsuleSet struct {
	LockedCapsules        []domain.LockedCapsule
	Components            map[string]domain.ResolvedComponent
	ProjectDigest         string
	Classification        domain.ResolutionClassification
	DiffSinceLastApproval map[string]string
}

func (workflow *profileResolveWorkflow) resolveCapsules(ctx context.Context, ownerID string, version ProfileVersionData, projectDigest *string, lastApproved map[string]string) (resolvedCapsuleSet, error) {
	refs := version.CapsuleRefs
	sources := make([]domain.CapsuleComponentSet, 0, len(refs))
	locked := make([]domain.LockedCapsule, 0, len(refs))
	diffSinceApproval := make(map[string]string)
	for _, ref := range refs {
		if err := validateOwnedCapsuleRef(ownerID, ref.Ref); err != nil {
			return resolvedCapsuleSet{}, restate.TerminalErrorf("%v", err)
		}
		if ref.FreshnessPolicy == domain.FreshnessPin && !strings.Contains(ref.Ref, "@sha256:") {
			return resolvedCapsuleSet{}, restate.TerminalErrorf("Capsule Ref %q uses pin freshness but is not an exact digest reference", ref.Ref)
		}
		resolution, err := workflow.resolver.Resolve(ctx, ownerID, ref)
		if err != nil {
			return resolvedCapsuleSet{}, fmt.Errorf("resolve Capsule Ref %q: %w", ref.Ref, err)
		}
		if err := validateCapsuleResolution(ownerID, ref.Ref, resolution); err != nil {
			return resolvedCapsuleSet{}, restate.TerminalErrorf("%v", err)
		}
		digest, err := exactOrResolvedDigest(ref.Ref, resolution.Digest)
		if err != nil {
			return resolvedCapsuleSet{}, err
		}
		if ref.FreshnessPolicy == domain.FreshnessReview {
			approved := lastApproved[ref.Ref]
			if approved == "" || approved != digest {
				diff := resolution.DiffSinceLastApproval
				if diff == "" {
					diff = fmt.Sprintf("Capsule digest changed from %q to %q", approved, digest)
				}
				diffSinceApproval[ref.Ref] = diff
			}
		}
		locked = append(locked, domain.LockedCapsule{Ref: ref.Ref, Digest: digest})
		sources = append(sources, domain.CapsuleComponentSet{Ref: ref.Ref, Digest: digest, Exclusions: ref.Exclusions, Components: resolution.Components})
	}
	emptyProjectDigest := "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	selectedProjectDigest := emptyProjectDigest
	var project *domain.CapsuleComponentSet
	if projectDigest != nil && strings.TrimSpace(*projectDigest) != "" {
		selectedProjectDigest = *projectDigest
		projectRef := domain.CapsuleRef{Ref: contracts.FormatOwnedCapsuleRef(ownerID, selectedProjectDigest), FreshnessPolicy: domain.FreshnessPin}
		resolution, err := workflow.resolver.Resolve(ctx, ownerID, projectRef)
		if err != nil {
			return resolvedCapsuleSet{}, fmt.Errorf("resolve project Capsule %q: %w", selectedProjectDigest, err)
		}
		if err := validateCapsuleResolution(ownerID, projectRef.Ref, resolution); err != nil {
			return resolvedCapsuleSet{}, restate.TerminalErrorf("%v", err)
		}
		digest, err := exactOrResolvedDigest(projectRef.Ref, resolution.Digest)
		if err != nil {
			return resolvedCapsuleSet{}, err
		}
		if digest != selectedProjectDigest {
			return resolvedCapsuleSet{}, fmt.Errorf("project Capsule digest resolved to %q, want %q", digest, selectedProjectDigest)
		}
		project = &domain.CapsuleComponentSet{Ref: projectRef.Ref, Digest: digest, Components: resolution.Components}
	}
	composition, err := domain.ResolveCapsuleComposition(sources, project)
	if err != nil {
		if workflow.metrics != nil && errors.Is(err, domain.ErrComponentConflict) {
			workflow.metrics.AddCounter(domain.MetricComponentConflictsTotal, 1)
		}
		return resolvedCapsuleSet{}, fmt.Errorf("resolve Components: %w", err)
	}
	result := make(map[string]domain.ResolvedComponent, len(composition.Components))
	for id, component := range composition.Components {
		capsuleDigest := composition.ComponentCapsuleDigests[id]
		if capsuleDigest == "" {
			capsuleDigest = selectedProjectDigest
		}
		result[id] = domain.ResolvedComponent{
			ID: id, Type: component.Type, CapsuleDigest: capsuleDigest, ComponentDigest: component.Digest,
			Scope: component.Scope, TrustClass: component.TrustClass, Requirements: component.Requirements,
			Provenance: cloneStringMapForWorkflow(component.Provenance), Sources: cloneResolvedComponentSourcesForWorkflow(composition.ComponentSources[id]),
		}
	}
	if len(diffSinceApproval) > 0 {
		composition.Classification = domain.RequiresReview
	}
	return resolvedCapsuleSet{
		LockedCapsules: locked, Components: result, ProjectDigest: selectedProjectDigest,
		Classification: composition.Classification, DiffSinceLastApproval: diffSinceApproval,
	}, nil
}

func classifyUpgrade(policy domain.UpgradePolicy, resolved resolvedCapsuleSet, managedTargets []domain.ManagedTargetState, supplied *domain.AutoSafePlan) (bool, bool, string) {
	plan := domain.AutoSafePlan{ManagedTargets: cloneManagedTargetStatesForWorkflow(managedTargets)}
	if supplied != nil {
		plan = *supplied
		// ManagedTargetsClean is an untrusted legacy verdict. Replace the
		// entire managed-target input with the state loaded from storage.
		plan.ManagedTargets = cloneManagedTargetStatesForWorkflow(managedTargets)
	}
	for _, component := range resolved.Components {
		switch {
		case component.TrustClass == domain.TrustExecutable:
			plan.ExecutableDigestChanged = true
		case component.TrustClass == domain.TrustPermission || component.Type == domain.ComponentPermissionPolicy:
			plan.PermissionChanged = true
		case component.Type == domain.ComponentIntegration:
			plan.IntegrationChanged = true
		case len(component.Requirements.Secrets) > 0:
			plan.CredentialRequirementChanged = true
		}
	}
	decision := domain.EvaluateAutoSafe(plan)
	switch policy {
	case domain.UpgradeManual:
		return false, true, "manual_upgrade"
	case domain.UpgradeNotify:
		if resolved.Classification != domain.AutoSafe || len(resolved.DiffSinceLastApproval) > 0 {
			return false, true, "requires_review"
		}
		return false, false, "notify_upgrade"
	case domain.UpgradeAutoSafe:
		if len(resolved.DiffSinceLastApproval) > 0 {
			return false, true, "freshness_review_required"
		}
		if resolved.Classification != domain.AutoSafe && decision.Reason == "" {
			return false, true, "requires_review"
		}
		if !decision.Applyable {
			return false, true, decision.Reason
		}
		return true, false, ""
	default:
		return false, true, "invalid_upgrade_policy"
	}
}

func cloneStringMapForWorkflow(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func cloneResolvedComponentSourcesForWorkflow(values []domain.ResolvedComponentSource) []domain.ResolvedComponentSource {
	if values == nil {
		return nil
	}
	return append([]domain.ResolvedComponentSource(nil), values...)
}

func cloneManagedTargetStatesForWorkflow(values []domain.ManagedTargetState) []domain.ManagedTargetState {
	if values == nil {
		return nil
	}
	return append([]domain.ManagedTargetState(nil), values...)
}

func formatApprovalDiffs(values map[string]string) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+": "+values[key])
	}
	return strings.Join(parts, "; ")
}

func validateOwnedCapsuleRef(ownerID, ref string) error {
	parsed, err := contracts.ParseOwnedCapsuleRef(ref)
	if err != nil {
		return err
	}
	if parsed.OwnerID != ownerID {
		return fmt.Errorf("Capsule Ref %q belongs to owner %q, want %q", ref, parsed.OwnerID, ownerID)
	}
	return nil
}

func validateCapsuleResolution(ownerID, ref string, resolution CapsuleResolution) error {
	if resolution.OwnerID != ownerID {
		return fmt.Errorf("Capsule Ref %q resolved from owner %q, want %q", ref, resolution.OwnerID, ownerID)
	}
	if _, err := contracts.ParseOwnedCapsuleRef(contracts.FormatOwnedCapsuleRef(ownerID, resolution.Digest)); err != nil {
		return fmt.Errorf("Capsule Ref %q resolved to an invalid owner-scoped digest: %w", ref, err)
	}
	return nil
}

func exactOrResolvedDigest(ref, resolved string) (string, error) {
	if at := strings.LastIndex(ref, "@sha256:"); at >= 0 {
		exact := ref[at+1:]
		if resolved != "" && resolved != exact {
			return "", fmt.Errorf("Capsule Ref %q resolved to %q, want pinned digest %q", ref, resolved, exact)
		}
		return exact, nil
	}
	if resolved == "" {
		return "", fmt.Errorf("Capsule Ref %q did not resolve to a digest", ref)
	}
	return resolved, nil
}

func filterExcludedComponents(components []domain.Component, exclusions []string) []domain.Component {
	if len(exclusions) == 0 {
		return components
	}
	excluded := make(map[string]struct{}, len(exclusions))
	for _, id := range exclusions {
		excluded[id] = struct{}{}
	}
	filtered := make([]domain.Component, 0, len(components))
	for _, component := range components {
		if _, omit := excluded[component.ID]; !omit {
			filtered = append(filtered, component)
		}
	}
	return filtered
}

type ProfileResolveClient struct {
	ingress *ingress.Client
}

func NewProfileResolveClient(client *ingress.Client) *ProfileResolveClient {
	return &ProfileResolveClient{ingress: client}
}

func (client *ProfileResolveClient) SendProfileResolve(ctx context.Context, input ProfileResolveInput) error {
	_, err := ingress.WorkflowSend[ProfileResolveInput](client.ingress, ProfileResolveService, input.OperationID, RunHandler).Send(ctx, input)
	return err
}
