package control

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/apps/guest"
	capsuleoci "github.com/ahmedhesham6/sshai/libs/capsule/oci"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/profile"
)

type SSHKeySource interface {
	SSHKeys(context.Context, Target) ([]guest.EnvironmentSSHKey, error)
}

type ManagedConfigurationSource interface {
	ManagedConfiguration(context.Context, Target) (guest.ProfileMaterializationBatch, error)
}

type SSHHostIdentityActivator interface {
	ActivateAndVerify(context.Context, Target, string) error
}

// AgentRequirement binds one selected agent executable to the exact version
// pinned into the platform-owned system image.
type AgentRequirement struct {
	Name            string
	Executable      string
	ExpectedVersion string
}

type LocalOperationsConfig struct {
	Target            Target
	Readiness         *guest.ReadinessReporter
	WorkspaceRoot     string
	HomeRoot          string
	CacheRoot         string
	PlatformRoot      string
	SSHDRoot          string
	DevUID            int
	DevGID            int
	AgentRequirements []AgentRequirement

	HostIdentityGenerator guest.SSHHostIdentityGenerator
	HostIdentityActivator SSHHostIdentityActivator
	SSHKeys               SSHKeySource
	ManagedConfiguration  ManagedConfigurationSource
	Activity              *guest.Observer
	Shutdown              func(context.Context) error
}

// LocalOperations binds the transport contract to guest's filesystem-safe
// Project Seed, SSH identity, Materialization, readiness, and activity code.
type LocalOperations struct {
	config LocalOperationsConfig
}

func NewLocalOperations(config LocalOperationsConfig) (*LocalOperations, error) {
	if config.Readiness == nil {
		return nil, errors.New("construct local guest operations: readiness reporter is required")
	}
	if config.Target.OwnerUserID == "" || config.Target.EnvironmentID == "" || config.Target.RuntimeID == "" || config.Target.ProviderID == "" || config.Target.PrivateIPv4 == "" {
		return nil, errors.New("construct local guest operations: owner, Environment, Runtime, provider, and private address are required")
	}
	if config.DevUID <= 0 || config.DevGID <= 0 {
		return nil, errors.New("construct local guest operations: positive dev UID and GID are required")
	}
	if config.HostIdentityActivator == nil {
		return nil, errors.New("construct local guest operations: SSH host identity activator is required")
	}
	for name, value := range map[string]string{
		"workspace": config.WorkspaceRoot, "home": config.HomeRoot, "cache": config.CacheRoot,
		"platform": config.PlatformRoot, "sshd": config.SSHDRoot,
	} {
		if value == "" {
			return nil, fmt.Errorf("construct local guest operations: %s root is required", name)
		}
	}
	if len(config.AgentRequirements) == 0 {
		return nil, errors.New("construct local guest operations: selected agent requirements are required")
	}
	seenNames := make(map[string]struct{}, len(config.AgentRequirements))
	seenExecutables := make(map[string]struct{}, len(config.AgentRequirements))
	for _, requirement := range config.AgentRequirements {
		if strings.TrimSpace(requirement.Name) == "" || strings.TrimSpace(requirement.ExpectedVersion) == "" ||
			!filepath.IsAbs(requirement.Executable) || filepath.Clean(requirement.Executable) != requirement.Executable {
			return nil, errors.New("construct local guest operations: selected agent name, exact version, and absolute clean executable are required")
		}
		if _, duplicate := seenNames[requirement.Name]; duplicate {
			return nil, errors.New("construct local guest operations: selected agent names must be unique")
		}
		if _, duplicate := seenExecutables[requirement.Executable]; duplicate {
			return nil, errors.New("construct local guest operations: selected agent executables must be unique")
		}
		seenNames[requirement.Name] = struct{}{}
		seenExecutables[requirement.Executable] = struct{}{}
	}
	config.AgentRequirements = append([]AgentRequirement(nil), config.AgentRequirements...)
	return &LocalOperations{config: config}, nil
}

func (operations *LocalOperations) Readiness(ctx context.Context, target Target) (ReadinessStatus, error) {
	if err := operations.validateTarget(target); err != nil {
		return ReadinessStatus{}, err
	}
	snapshot, err := operations.config.Readiness.Snapshot(ctx)
	if err != nil {
		return ReadinessStatus{}, err
	}
	return ReadinessStatus{Snapshot: snapshot, PrivateIPv4: operations.config.Target.PrivateIPv4}, nil
}

func (operations *LocalOperations) ApplyProjectSeed(ctx context.Context, request ProjectSeedRequest) error {
	if err := operations.validateTarget(request.Target); err != nil {
		return err
	}
	if err := operations.canAdvance(ctx, guest.ReadinessProjectReady); err != nil {
		return err
	}
	application, err := guest.NewProjectSeedApplication(request.Seed)
	if err != nil {
		return permanentOperationError(fmt.Errorf("prepare Project Seed: %w", err))
	}
	if err := application.Apply(ctx, operations.config.WorkspaceRoot); err != nil {
		return classifyImmutableGuestInput(err)
	}
	if err := chownUserTree(operations.config.WorkspaceRoot, operations.config.DevUID, operations.config.DevGID); err != nil {
		return fmt.Errorf("set Project Seed ownership: %w", err)
	}
	return operations.advance(ctx, guest.ReadinessProjectReady)
}

func (operations *LocalOperations) RestoreSSHHostIdentity(ctx context.Context, target Target) (guest.SSHHostIdentityStatus, error) {
	if err := operations.validateTarget(target); err != nil {
		return guest.SSHHostIdentityStatus{}, err
	}
	status, err := guest.ReconcileSSHHostIdentity(ctx, guest.SSHHostIdentityRequest{
		PlatformRoot: operations.config.PlatformRoot, SSHDRoot: operations.config.SSHDRoot,
	}, operations.config.HostIdentityGenerator)
	if err != nil {
		return guest.SSHHostIdentityStatus{}, err
	}
	if err := operations.config.HostIdentityActivator.ActivateAndVerify(ctx, target, status.Fingerprint); err != nil {
		return guest.SSHHostIdentityStatus{}, fmt.Errorf("activate SSH host identity: %w", err)
	}
	if err := operations.advance(ctx, guest.ReadinessSSHReady); err != nil {
		return guest.SSHHostIdentityStatus{}, err
	}
	return status, nil
}

func (operations *LocalOperations) ReconcileSSHKeys(ctx context.Context, target Target) error {
	if err := operations.validateTarget(target); err != nil {
		return err
	}
	if operations.config.SSHKeys == nil {
		return permanentOperationError(errors.New("reconcile SSH keys: key source is not configured"))
	}
	keys, err := operations.config.SSHKeys.SSHKeys(ctx, target)
	if err != nil {
		return fmt.Errorf("load SSH keys: %w", err)
	}
	_, err = guest.ReconcileDevAuthorizedKeys(guest.AuthorizedKeysRequest{HomeRoot: operations.config.HomeRoot, Keys: keys})
	if err != nil {
		return err
	}
	return chownUserTree(filepath.Join(operations.config.HomeRoot, ".ssh"), operations.config.DevUID, operations.config.DevGID)
}

func (operations *LocalOperations) ReconcileManagedConfiguration(ctx context.Context, target Target) error {
	if err := operations.validateTarget(target); err != nil {
		return err
	}
	if operations.config.ManagedConfiguration == nil {
		return permanentOperationError(errors.New("reconcile managed configuration: source is not configured"))
	}
	batch, err := operations.config.ManagedConfiguration.ManagedConfiguration(ctx, target)
	if err != nil {
		return fmt.Errorf("load managed configuration: %w", err)
	}
	batch.HomeRoot = operations.config.HomeRoot
	batch.WorkspaceRoot = operations.config.WorkspaceRoot
	_, err = guest.ApplyProfileMaterializations(batch)
	if err != nil {
		return err
	}
	return operations.chownUserRoots()
}

func (operations *LocalOperations) PrepareShutdown(ctx context.Context, target Target) error {
	if err := operations.validateTarget(target); err != nil {
		return err
	}
	if operations.config.Shutdown == nil {
		return permanentOperationError(errors.New("prepare Runtime shutdown: shutdown preparer is not configured"))
	}
	return operations.config.Shutdown(ctx)
}

func (operations *LocalOperations) ApplyMaterialization(ctx context.Context, request MaterializationRequest) ([]profile.ProfileMaterializationResult, error) {
	if err := operations.validateTarget(request.Target); err != nil {
		return nil, err
	}
	if request.Lock.EnvironmentID != operations.config.Target.EnvironmentID {
		return nil, permanentOperationError(errors.New("materialize Capsule Lock: Lock belongs to another Environment"))
	}
	if subtle.ConstantTimeCompare([]byte(request.OwnerID), []byte(operations.config.Target.OwnerUserID)) != 1 {
		return nil, permanentOperationError(errors.New("materialize Capsule Lock: owner does not match the Environment"))
	}
	lock, err := domain.NewCapsuleLock(request.Lock)
	if err != nil {
		return nil, permanentOperationError(err)
	}
	if err := operations.canAdvance(ctx, guest.ReadinessAgentsValidated); err != nil {
		return nil, err
	}
	var grants capsuleoci.GrantProvider
	if len(request.ReadGrants) > 0 {
		grants, err = newReadGrantProvider(request.ReadGrants)
		if err != nil {
			return nil, err
		}
	}
	results, err := guest.MaterializeCapsuleLock(ctx, profile.CapsuleLockMaterializationBatch{
		Lock: lock, OwnerID: request.OwnerID, Grants: grants,
		CacheRoot: operations.config.CacheRoot, HomeRoot: operations.config.HomeRoot, WorkspaceRoot: operations.config.WorkspaceRoot,
		Intent: request.Intent, Installed: request.Installed, Approvals: request.Approvals,
		AdapterID: request.AdapterID, TargetAgentVersion: request.TargetAgentVersion,
		NonSecretOverridesDigest: request.NonSecretOverridesDigest,
		SecretVersionIdentifiers: append([]string(nil), request.SecretVersionIdentifiers...),
	})
	if err != nil {
		if errors.Is(err, guest.ErrProfileMaterializationBlocked) {
			return results, permanentOperationError(err)
		}
		return nil, classifyImmutableGuestInput(err)
	}
	if err := operations.chownUserRoots(); err != nil {
		return nil, fmt.Errorf("set Materialization ownership: %w", err)
	}
	if err := operations.advance(ctx, guest.ReadinessAgentsValidated); err != nil {
		return nil, err
	}
	return results, nil
}

func classifyImmutableGuestInput(err error) error {
	if errors.Is(err, guest.ErrProjectSeedWorkspaceDiverged) || errors.Is(err, guest.ErrProjectSeedContentInvalid) || errors.Is(err, guest.ErrCapsuleContentInvalid) {
		return permanentOperationError(err)
	}
	return err
}

func (operations *LocalOperations) ValidateToolchain(ctx context.Context, target Target) error {
	if err := operations.validateTarget(target); err != nil {
		return err
	}
	if err := validateAgentExecutables(ctx, operations.config.AgentRequirements); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return permanentOperationError(fmt.Errorf("validate selected agent toolchain: %w", err))
	}
	// Project toolchain requirements are not represented by the Project domain
	// yet. S9 must extend this boundary once that aggregate exposes them.
	return nil
}

func validateAgentExecutables(ctx context.Context, requirements []AgentRequirement) error {
	if len(requirements) == 0 {
		return errors.New("at least one selected agent requirement is required")
	}
	seen := make(map[string]struct{}, len(requirements))
	for _, requirement := range requirements {
		executable := requirement.Executable
		if strings.TrimSpace(requirement.Name) == "" || strings.TrimSpace(requirement.ExpectedVersion) == "" || !filepath.IsAbs(executable) || filepath.Clean(executable) != executable {
			return fmt.Errorf("agent requirement %q is incomplete", requirement.Name)
		}
		if _, duplicate := seen[executable]; duplicate {
			return fmt.Errorf("agent executable path %q is duplicated", executable)
		}
		seen[executable] = struct{}{}
		info, err := os.Stat(executable)
		if err != nil {
			return fmt.Errorf("inspect agent executable %q: %w", executable, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("agent executable %q is not an executable file", executable)
		}
		versionContext, cancel := context.WithTimeout(ctx, 30*time.Second)
		output, commandErr := exec.CommandContext(versionContext, executable, "--version").CombinedOutput()
		cancel()
		if commandErr != nil {
			return fmt.Errorf("read %s agent version: %w", requirement.Name, commandErr)
		}
		if !containsExactVersion(string(output), requirement.ExpectedVersion) {
			return fmt.Errorf("%s agent version differs from pinned version %q", requirement.Name, requirement.ExpectedVersion)
		}
	}
	return nil
}

func containsExactVersion(output, expected string) bool {
	for offset := 0; offset <= len(output)-len(expected); {
		index := strings.Index(output[offset:], expected)
		if index < 0 {
			return false
		}
		index += offset
		leftBoundary := index == 0 || !isVersionCharacter(output[index-1])
		right := index + len(expected)
		rightBoundary := right == len(output) || !isVersionCharacter(output[right])
		if leftBoundary && rightBoundary {
			return true
		}
		offset = index + 1
	}
	return false
}

func isVersionCharacter(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' || value == '.' || value == '+' || value == '-'
}

type readGrantProvider struct {
	grants map[string]ReadGrant
	client *http.Client
}

func newReadGrantProvider(grants map[string]ReadGrant) (*readGrantProvider, error) {
	validated := make(map[string]ReadGrant, len(grants))
	for key, grant := range grants {
		parsed, err := url.Parse(grant.URL)
		if key == "" || err != nil || parsed.Scheme != "https" || parsed.Host == "" || grant.ExpiresAt.IsZero() {
			return nil, permanentOperationError(errors.New("materialize Capsule Lock: read grant is invalid"))
		}
		validated[key] = grant
	}
	return &readGrantProvider{grants: validated, client: &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}, nil
}

func (provider *readGrantProvider) Grant(ctx context.Context, request capsuleoci.GrantRequest) (capsuleoci.Grant, error) {
	if request.Operation != capsuleoci.GrantRead {
		return capsuleoci.Grant{}, permanentOperationError(errors.New("Capsule grant permits reads only"))
	}
	grant, present := provider.grants[request.Key]
	if !present {
		return capsuleoci.Grant{}, permanentOperationError(errors.New("Capsule read grant is absent"))
	}
	if !time.Now().Before(grant.ExpiresAt) {
		return capsuleoci.Grant{}, transientOperationError(errors.New("Capsule read grant is expired"))
	}
	return capsuleoci.Grant{
		ExpiresAt: grant.ExpiresAt,
		Read: func(readContext context.Context) (io.ReadCloser, error) {
			httpRequest, err := http.NewRequestWithContext(readContext, http.MethodGet, grant.URL, nil)
			if err != nil {
				return nil, errors.New("construct Capsule read request")
			}
			response, err := provider.client.Do(httpRequest)
			if err != nil {
				return nil, errors.New("execute Capsule read request")
			}
			if response.StatusCode != http.StatusOK {
				response.Body.Close()
				return nil, grantHTTPError{status: response.StatusCode}
			}
			return response.Body, nil
		},
	}, nil
}

type grantHTTPError struct{ status int }

func (err grantHTTPError) Error() string {
	return fmt.Sprintf("Capsule read returned HTTP %d", err.status)
}
func (err grantHTTPError) StatusCode() int { return err.status }
func (err grantHTTPError) Transient() bool {
	return err.status == http.StatusForbidden || err.status == http.StatusRequestTimeout || err.status == http.StatusTooManyRequests || err.status >= 500
}

func (operations *LocalOperations) chownUserRoots() error {
	for _, root := range []string{operations.config.HomeRoot, operations.config.WorkspaceRoot} {
		if err := chownUserTree(root, operations.config.DevUID, operations.config.DevGID); err != nil {
			return err
		}
	}
	return nil
}

func chownUserTree(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(name, uid, gid)
	})
}

func (operations *LocalOperations) ReadActivitySnapshot(ctx context.Context, target Target) (ActivitySnapshot, error) {
	if err := operations.validateTarget(target); err != nil {
		return ActivitySnapshot{}, err
	}
	if operations.config.Activity == nil {
		return ActivitySnapshot{}, permanentOperationError(errors.New("read Activity Snapshot: observer is not configured"))
	}
	snapshot, err := operations.config.Activity.Observe(ctx)
	if err != nil {
		return ActivitySnapshot{}, err
	}
	return ActivitySnapshot{
		RuntimeID: snapshot.RuntimeID(), ObservedAt: snapshot.ObservedAt(), GuestSequence: snapshot.GuestSequence(),
		SSHConnections: snapshot.SSHConnections(), CodexProcesses: snapshot.CodexProcesses(), ClaudeProcesses: snapshot.ClaudeProcesses(),
		ProtectedProcesses: snapshot.ProtectedProcesses(), SelectedContainers: snapshot.SelectedContainers(),
		UnknownUserProcesses: snapshot.UnknownUserProcesses(), UserSessionProcesses: snapshot.UserSessionProcesses(),
		EscapedUserProcesses: snapshot.EscapedUserProcesses(),
	}, nil
}

func (operations *LocalOperations) validateTarget(target Target) error {
	for _, identity := range []struct{ name, expected, actual string }{
		{name: "owner", expected: operations.config.Target.OwnerUserID, actual: target.OwnerUserID},
		{name: "Environment", expected: operations.config.Target.EnvironmentID, actual: target.EnvironmentID},
		{name: "Runtime", expected: operations.config.Target.RuntimeID, actual: target.RuntimeID},
		{name: "provider", expected: operations.config.Target.ProviderID, actual: target.ProviderID},
	} {
		if subtle.ConstantTimeCompare([]byte(identity.expected), []byte(identity.actual)) != 1 {
			return permanentOperationError(fmt.Errorf("guest control request %s identity does not match the current boot", identity.name))
		}
	}
	if target.PrivateIPv4 != "" && subtle.ConstantTimeCompare([]byte(operations.config.Target.PrivateIPv4), []byte(target.PrivateIPv4)) != 1 {
		return permanentOperationError(errors.New("guest control request private address does not match the current boot"))
	}
	return nil
}

func (operations *LocalOperations) advance(ctx context.Context, desired guest.ReadinessLevel) error {
	current, err := operations.config.Readiness.Snapshot(ctx)
	if err != nil {
		return err
	}
	currentOrder, desiredOrder := readinessOrder(current.Level), readinessOrder(desired)
	if currentOrder >= desiredOrder {
		return nil
	}
	if currentOrder+1 != desiredOrder {
		return fmt.Errorf("advance guest readiness: %s requires the preceding readiness level", desired)
	}
	_, err = operations.config.Readiness.Advance(ctx, desired, time.Now().UTC())
	return err
}

func (operations *LocalOperations) canAdvance(ctx context.Context, desired guest.ReadinessLevel) error {
	current, err := operations.config.Readiness.Snapshot(ctx)
	if err != nil {
		return err
	}
	currentOrder, desiredOrder := readinessOrder(current.Level), readinessOrder(desired)
	if currentOrder >= desiredOrder || currentOrder+1 == desiredOrder {
		return nil
	}
	return permanentOperationError(fmt.Errorf("advance guest readiness: %s requires the preceding readiness level", desired))
}

var _ Operations = (*LocalOperations)(nil)
