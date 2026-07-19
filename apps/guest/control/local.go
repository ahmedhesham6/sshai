package control

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

type LocalOperationsConfig struct {
	Target        Target
	Readiness     *guest.ReadinessReporter
	WorkspaceRoot string
	HomeRoot      string
	CacheRoot     string
	PlatformRoot  string
	SSHDRoot      string

	HostIdentityGenerator guest.SSHHostIdentityGenerator
	SSHKeys               SSHKeySource
	ManagedConfiguration  ManagedConfigurationSource
	CapsuleGrants         capsuleoci.GrantProvider
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
	for name, value := range map[string]string{
		"workspace": config.WorkspaceRoot, "home": config.HomeRoot, "cache": config.CacheRoot,
		"platform": config.PlatformRoot, "sshd": config.SSHDRoot,
	} {
		if value == "" {
			return nil, fmt.Errorf("construct local guest operations: %s root is required", name)
		}
	}
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
	application, err := guest.NewProjectSeedApplication(request.Seed)
	if err != nil {
		return fmt.Errorf("prepare Project Seed: %w", err)
	}
	if err := application.Apply(ctx, operations.config.WorkspaceRoot); err != nil {
		return err
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
		return errors.New("reconcile SSH keys: key source is not configured")
	}
	keys, err := operations.config.SSHKeys.SSHKeys(ctx, target)
	if err != nil {
		return fmt.Errorf("load SSH keys: %w", err)
	}
	_, err = guest.ReconcileDevAuthorizedKeys(guest.AuthorizedKeysRequest{HomeRoot: operations.config.HomeRoot, Keys: keys})
	return err
}

func (operations *LocalOperations) ReconcileManagedConfiguration(ctx context.Context, target Target) error {
	if err := operations.validateTarget(target); err != nil {
		return err
	}
	if operations.config.ManagedConfiguration == nil {
		return errors.New("reconcile managed configuration: source is not configured")
	}
	batch, err := operations.config.ManagedConfiguration.ManagedConfiguration(ctx, target)
	if err != nil {
		return fmt.Errorf("load managed configuration: %w", err)
	}
	batch.HomeRoot = operations.config.HomeRoot
	batch.WorkspaceRoot = operations.config.WorkspaceRoot
	_, err = guest.ApplyProfileMaterializations(batch)
	return err
}

func (operations *LocalOperations) PrepareShutdown(ctx context.Context, target Target) error {
	if err := operations.validateTarget(target); err != nil {
		return err
	}
	if operations.config.Shutdown == nil {
		return errors.New("prepare Runtime shutdown: shutdown preparer is not configured")
	}
	return operations.config.Shutdown(ctx)
}

func (operations *LocalOperations) ApplyMaterialization(ctx context.Context, request MaterializationRequest) ([]profile.ProfileMaterializationResult, error) {
	if err := operations.validateTarget(request.Target); err != nil {
		return nil, err
	}
	if request.Lock.EnvironmentID != operations.config.Target.EnvironmentID {
		return nil, errors.New("materialize Capsule Lock: Lock belongs to another Environment")
	}
	if subtle.ConstantTimeCompare([]byte(request.OwnerID), []byte(operations.config.Target.OwnerUserID)) != 1 {
		return nil, errors.New("materialize Capsule Lock: owner does not match the Environment")
	}
	lock, err := domain.NewCapsuleLock(request.Lock)
	if err != nil {
		return nil, err
	}
	grants := operations.config.CapsuleGrants
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
		return nil, err
	}
	if err := operations.advance(ctx, guest.ReadinessAgentsValidated); err != nil {
		return nil, err
	}
	return results, nil
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
			return nil, errors.New("materialize Capsule Lock: read grant is invalid")
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
		return capsuleoci.Grant{}, errors.New("Capsule grant permits reads only")
	}
	grant, present := provider.grants[request.Key]
	if !present || !time.Now().Before(grant.ExpiresAt) {
		return capsuleoci.Grant{}, errors.New("Capsule read grant is absent or expired")
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
				return nil, fmt.Errorf("Capsule read returned HTTP %d", response.StatusCode)
			}
			return response.Body, nil
		},
	}, nil
}

func (operations *LocalOperations) ReadActivitySnapshot(ctx context.Context, target Target) (ActivitySnapshot, error) {
	if err := operations.validateTarget(target); err != nil {
		return ActivitySnapshot{}, err
	}
	if operations.config.Activity == nil {
		return ActivitySnapshot{}, errors.New("read Activity Snapshot: observer is not configured")
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
			return fmt.Errorf("guest control request %s identity does not match the current boot", identity.name)
		}
	}
	if target.PrivateIPv4 != "" && subtle.ConstantTimeCompare([]byte(operations.config.Target.PrivateIPv4), []byte(target.PrivateIPv4)) != 1 {
		return errors.New("guest control request private address does not match the current boot")
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

var _ Operations = (*LocalOperations)(nil)
