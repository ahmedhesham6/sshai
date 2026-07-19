// Package control provides the mutually authenticated guest control channel.
package control

import (
	"context"
	"time"

	"github.com/ahmedhesham6/sshai/apps/guest"
	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/profile"
)

const (
	readinessPath              = "/v1/readiness"
	projectSeedPath            = "/v1/project-seed/apply"
	sshHostIdentityPath        = "/v1/ssh/host-identity/restore"
	sshKeysPath                = "/v1/ssh/authorized-keys/reconcile"
	managedConfigurationPath   = "/v1/managed-configuration/reconcile"
	shutdownPath               = "/v1/shutdown/prepare"
	materializationPath        = "/v1/materialization/apply"
	toolchainValidationPath    = "/v1/toolchain/validate"
	activitySnapshotPath       = "/v1/activity-snapshot"
	defaultMaximumRequestBytes = application.ProjectSeedTransportMaximumRequestBytes
)

// Target binds every control request to the Environment and current Runtime
// for which the workflow was invoked.
type Target struct {
	OwnerUserID   string `json:"ownerUserId"`
	EnvironmentID string `json:"environmentId"`
	RuntimeID     string `json:"runtimeId"`
	ProviderID    string `json:"providerId"`
	PrivateIPv4   string `json:"privateIPv4,omitempty"`
}

type ReadinessRequest struct {
	Target Target `json:"target"`
}

type ReadinessStatus struct {
	Snapshot    guest.ReadinessSnapshot `json:"snapshot"`
	PrivateIPv4 string                  `json:"privateIPv4"`
}

type ProjectSeedRequest struct {
	Target Target                            `json:"target"`
	Seed   guest.ProjectSeedApplicationInput `json:"seed"`
}

// MaterializationRequest contains serializable Capsule Lock intent and
// object-specific read capabilities. Filesystem roots remain guest-owned
// server configuration and cannot be selected by a remote caller.
type MaterializationRequest struct {
	Target  Target                     `json:"target"`
	Lock    domain.CapsuleLockSnapshot `json:"capsuleLock"`
	OwnerID string                     `json:"ownerId"`
	Intent  profile.PlanIntent         `json:"intent"`

	Installed []profile.InstalledMaterialization `json:"installed,omitempty"`
	Approvals map[string]profile.ApprovalMarker  `json:"approvals,omitempty"`

	AdapterID                string               `json:"adapterId,omitempty"`
	TargetAgentVersion       string               `json:"targetAgentVersion,omitempty"`
	NonSecretOverridesDigest string               `json:"nonSecretOverridesDigest,omitempty"`
	SecretVersionIdentifiers []string             `json:"secretVersionIdentifiers,omitempty"`
	ReadGrants               map[string]ReadGrant `json:"readGrants,omitempty"`
}

// ReadGrant is one short-lived, object-specific Capsule capability. URL is
// intentionally never included in errors or logs because its query may carry
// signing material.
type ReadGrant struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type ActivitySnapshot struct {
	RuntimeID            string    `json:"runtimeId"`
	ObservedAt           time.Time `json:"observedAt"`
	GuestSequence        int64     `json:"guestSequence"`
	SSHConnections       int       `json:"sshConnections"`
	CodexProcesses       int       `json:"codexProcesses"`
	ClaudeProcesses      int       `json:"claudeProcesses"`
	ProtectedProcesses   int       `json:"protectedProcesses"`
	SelectedContainers   int       `json:"selectedContainers"`
	UnknownUserProcesses int       `json:"unknownUserProcesses"`
	UserSessionProcesses int       `json:"userSessionProcesses"`
	EscapedUserProcesses int       `json:"escapedUserProcesses"`
}

// Operations is the guest supervisor boundary served over mTLS.
type Operations interface {
	Readiness(context.Context, Target) (ReadinessStatus, error)
	ApplyProjectSeed(context.Context, ProjectSeedRequest) error
	RestoreSSHHostIdentity(context.Context, Target) (guest.SSHHostIdentityStatus, error)
	ReconcileSSHKeys(context.Context, Target) error
	ReconcileManagedConfiguration(context.Context, Target) error
	PrepareShutdown(context.Context, Target) error
	ApplyMaterialization(context.Context, MaterializationRequest) ([]profile.ProfileMaterializationResult, error)
	ValidateToolchain(context.Context, Target) error
	ReadActivitySnapshot(context.Context, Target) (ActivitySnapshot, error)
}

type emptyResponse struct{}

type targetRequest struct {
	Target Target `json:"target"`
}

type sshHostIdentityResponse struct {
	Status guest.SSHHostIdentityStatus `json:"status"`
}

type materializationResponse struct {
	Results []profile.ProfileMaterializationResult `json:"results"`
}

type activitySnapshotResponse struct {
	Snapshot ActivitySnapshot `json:"snapshot"`
}

type errorResponse struct {
	Error   string                                 `json:"error"`
	Results []profile.ProfileMaterializationResult `json:"results,omitempty"`
}

type operationError struct {
	err       error
	transient bool
}

func (err operationError) Error() string   { return err.err.Error() }
func (err operationError) Unwrap() error   { return err.err }
func (err operationError) Transient() bool { return err.transient }

func permanentOperationError(err error) error {
	if err == nil {
		return nil
	}
	return operationError{err: err}
}

func transientOperationError(err error) error {
	if err == nil {
		return nil
	}
	return operationError{err: err, transient: true}
}
