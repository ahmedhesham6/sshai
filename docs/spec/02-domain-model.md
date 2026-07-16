# Domain model

The model separates durable user intent, persistent filesystem state, temporary compute, durable execution, and billing. Provider identifiers never define product identity.

## Aggregate relationships

```text
User
├── Profiles
│   └── Profile Versions
│       └── ordered Capsule Refs
├── SSH Keys
├── Subscription
│   ├── Credit Balance
│   └── Credit Transactions
└── Environments
    ├── Project Binding
    ├── Project Spec
    ├── Project Seed
    ├── pinned Profile Version
    ├── project Capsule
    ├── Capsule Lock
    ├── Materializations
    ├── State Components
    ├── Auto-stop Policy
    ├── Credential Bindings
    ├── current Runtime (zero or one)
    │   ├── Provider Resources
    │   ├── Readiness Reports
    │   └── Activity Snapshots
    ├── Operations
    └── Audit Events
```

## Core entities

### User

```go
type User struct {
	ID             string
	WorkOSUserID   string
	DefaultRegion  string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
```

WorkOS is authoritative for authentication identity. The local User is the product owner for Profiles, Environments, SSH Keys, and billing projections.

### Profile, ProfileVersion, and CapsuleRef

```go
type Profile struct {
	ID                 string
	OwnerUserID        string
	Name               string
	Slug               string
	CreatedAt          time.Time
	ArchivedAt         *time.Time
}

type ProfileVersion struct {
	ID               string
	ProfileID        string
	ParentVersionID  *string
	Version          int64
	Digest           string
	CapsuleRefs      []CapsuleRef
	CreatedAt        time.Time
}

type CapsuleRef struct {
	Ref              string
	FreshnessPolicy  FreshnessPolicy
	Exclusions       []string
}
```

Invariants:

- `(owner_user_id, slug)` is unique for non-archived Profiles.
- Profile Versions are immutable.
- A Profile Version contains an ordered list of Capsule Refs and no Capsule content.
- `parent_version_id` creates a linear history for a Profile.
- Publication uses an expected head version; a stale writer must refresh or fork.
- A fresh CLI installation may read an existing Profile, fork it, or create another. There is no Device entity.
- Capsule composition is flat. There are no profiles-of-profiles.

`FreshnessPolicy` is `track | review | pin`. Capsule order defines merge precedence for mergeable configuration. Identical component IDs with identical digests deduplicate; identical component IDs with different digests are conflicts until exclusions or reordering resolve them. Permission policies never merge silently.

### Component

```go
type Component struct {
	ID           string
	Type         ComponentType
	MediaType    string
	Digest       string
	SizeBytes    int64
	Scope        ComponentScope
	TrustClass   TrustClass
	Requirements ComponentRequirements
}

type ComponentRequirements struct {
	Commands []string
	Secrets  []string
}
```

Component types are `config | skill | command | subagent | hook | integration | permission-policy | template | extension`. A Component ID is stable within a Capsule, such as `skill:fix-ci`. Each Component has one OCI layer, and `Digest` identifies that layer. `Scope` is `user | project`. `TrustClass` is `declarative | executable | permission`. `integration` is the MCP component type.

Capsules are immutable OCI artifacts addressed by digest. The registry owns Capsule layers and manifest content; PostgreSQL stores Capsule Refs, composition, Locks, and Component metadata required for product state. A changed executable digest requires renewed review. `permission-policy` Components always require explicit itemized consent at apply time and are never `auto_safe`.

### ProjectBinding, ProjectSpec, and ProjectSeed

```go
type ProjectBinding struct {
	ID               string
	EnvironmentID    string
	RepositoryHost   string
	RepositoryOwner  string
	RepositoryName   string
	CanonicalURL     string
	WorkspacePath    string
}

type ProjectSpec struct {
	ID             string
	EnvironmentID  string
	Digest         string
	BaseRevision   string
	DetectedAt     time.Time
	Content        json.RawMessage
}

type ProjectSeed struct {
	ID                 string
	OwnerUserID        string
	EnvironmentID      *string
	BaseRevision       string
	GitBundleDigest    *string
	TrackedPatchDigest *string
	UntrackedBundleDigest *string
	ManifestDigest     string
	CreatedAt          time.Time
}
```

An Environment has exactly one Project Binding. A Project Seed is registered before Environment creation, belongs to one User, and may be assigned to one Environment exactly once. It is not a synchronization mechanism.

### Environment

```go
type Environment struct {
	ID                     string
	OwnerUserID            string
	Name                   string
	Slug                   string
	Lifecycle              EnvironmentLifecycle
	Health                 EnvironmentHealth
	Region                 string
	AvailabilityZone       string
	RuntimePreset          string
	PinnedProfileVersionID string
	CapsuleLockID          string
	UpgradePolicy          UpgradePolicy
	CurrentRuntimeID       *string
	AutoStopPolicyID       string
	CreatedAt              time.Time
	UpdatedAt              time.Time
	DeletedAt              *time.Time
	Version                int64
}
```

`EnvironmentLifecycle` is `creating | active | deleting | deleted`.
`EnvironmentHealth` is `healthy | degraded | blocked | unknown`.
`UpgradePolicy` is `manual | notify | auto_safe`.

Invariants:

- `(owner_user_id, slug)` is unique for non-deleted Environments.
- Region is immutable in the MVP.
- The primary Project Binding is immutable after creation except for an explicit future relink operation.
- An Environment has zero or one current Runtime.
- An Environment has at most one active mutating Operation.
- An Environment pins exactly one immutable Profile Version and one immutable Capsule Lock.
- The Capsule Lock resolves the pinned Profile Version together with the Environment's reviewed project Capsule. Environments materialize only from the Capsule Lock.
- The Capsule Lock is the reproducibility anchor; a pinned Profile Version alone is not sufficient.

### CapsuleLock

```go
type LockedCapsule struct {
	Ref    string
	Digest string
}

type ResolvedComponent struct {
	ID             string
	CapsuleDigest  string
	ComponentDigest string
	Scope          ComponentScope
	TrustClass     TrustClass
}

type CapsuleLock struct {
	ID                  string
	EnvironmentID       string
	ProfileVersionID    string
	ProjectCapsuleDigest string
	Capsules            []LockedCapsule
	ResolvedComponents  map[string]ResolvedComponent
	Digest              string
	CreatedAt           time.Time
}
```

A Capsule Lock is immutable and content-addressed. It records the exact Capsule digests selected from the ordered Capsule Refs, the Environment's project Capsule digest, and the resolved Component map. `CapsuleLock.ProfileVersionID` must equal `Environment.PinnedProfileVersionID`.

### StateComponent

```go
type StateComponent struct {
	ID              string
	EnvironmentID   string
	Kind            StateComponentKind
	Durability      DurabilityClass
	MountPath       string
	BackendResourceID string
	Health          string
	ObservedDigest  *string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}
```

Kinds are `workspace | home | services | cache`. Durability is `durable | disposable`. Logical components may share one physical EBS data volume.

### Runtime

```go
type Runtime struct {
	ID                  string
	EnvironmentID       string
	Sequence            int64
	Status              RuntimeStatus
	RuntimePreset       string
	Region              string
	AvailabilityZone    string
	ImageVersion        string
	ProviderInstanceRef *string
	PrivateAddress      *string
	StartedAt           *time.Time
	StoppedAt           *time.Time
	RetiredAt           *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}
```

`RuntimeStatus` is `absent | provisioning | starting | ready | stopping | stopped | replacing | error`.

Historical Runtimes remain for audit and usage attribution. Only the current Runtime may attach writable durable state.

### AutoStopPolicy and ActivitySnapshot

```go
type AutoStopPolicy struct {
	ID             string
	EnvironmentID  string
	Mode           AutoStopMode
	GracePeriod    time.Duration
	Configuration  json.RawMessage
}

type ActivitySnapshot struct {
	ID                    string
	RuntimeID             string
	ObservedAt            time.Time
	SSHConnections        int
	IDEConnections        int
	CodexProcesses        int
	ClaudeProcesses       int
	ProtectedProcesses    int
	SelectedContainers    int
	UnknownUserProcesses  int
	GuestSequence         int64
}
```

Initial modes are `when_disconnected | when_agents_finish | when_fully_idle | manual`. No Session aggregate exists in the MVP.

An Environment has exactly one policy row. A Profile Version may carry default policy values, which are copied into the Environment-specific policy at creation rather than referenced live.

### Materialization

```go
type Materialization struct {
	ID                         string
	EnvironmentID              string
	LockID                     string
	CapsuleDigest              string
	ComponentID                string
	ComponentDigest            string
	AdapterID                  string
	AdapterVersion             string
	TargetAgentVersion         string
	Scope                      ComponentScope
	NonSecretOverridesDigest   string
	SecretVersionIdentifiers   []string
	EffectiveCacheKey          string
	Mode                       MaterializationMode
	Target                     string
	Selector                   *string
	LastAppliedDigest          *string
	ObservedDigest             *string
	Status                     string
	AppliedAt                  *time.Time
}
```

Materialization identity is `(lock, capsule digest, component id, adapter)`, represented by `LockID`, `CapsuleDigest`, `ComponentID`, and `AdapterID`. The effective cache key is `(ComponentDigest, AdapterID, AdapterVersion, TargetAgentVersion, Scope, NonSecretOverridesDigest, SecretVersionIdentifiers)`. Resolved secret values never enter the key or any record.

Modes:

- `managed`: updates use three-way comparison and remote drift blocks overwrite.
- `seeded`: created once, then owned entirely by the Environment.
- `referenced`: resolved from another system at use time.

### CredentialRequirement and CredentialBinding

```go
type CredentialRequirement struct {
	ID               string
	ComponentID      *string
	ProjectSpecID     *string
	Kind             string
	Scope            string
	Name             string
	Reference        *string
}

type CredentialBinding struct {
	ID                      string
	EnvironmentID           string
	CredentialRequirementID string
	Method                  string
	ExternalReference       *string
	Status                  string
	ExpiresAt               *time.Time
}
```

Components and Project Specs declare Credential Requirements and secret references, never secret values or copied authentication caches. Environments hold Credential Bindings.
Project-scope Components are `seeded` only: they are applied at creation and then owned by the workspace, never `managed` inside `/workspace`.

### SSHKey

```go
type SSHKey struct {
	ID          string
	OwnerUserID string
	Label       string
	Algorithm   string
	Fingerprint string
	PublicKey   string
	CreatedAt   time.Time
	RevokedAt   *time.Time
}
```

Private key paths are local CLI configuration and are not sent to the service.

### ProviderResource

```go
type ProviderResource struct {
	ID             string
	EnvironmentID  string
	RuntimeID      *string
	StateComponentID *string
	OperationID    *string
	Provider       string
	Region         string
	ResourceType   string
	ProviderID     string
	Metadata       json.RawMessage
	CreatedAt      time.Time
	DeletedAt      *time.Time
}
```

The resource inventory replaces provider-ID columns on Environment and Runtime aggregates.

### Operation and OperationStep

```go
type Operation struct {
	ID                  string
	EnvironmentID       string
	Type                string
	Status              string
	RequestedByUserID   string
	IdempotencyKey      string
	RestateInvocationID string
	Input               json.RawMessage
	ErrorCode           *string
	ErrorMessage        *string
	CreatedAt           time.Time
	CompletedAt         *time.Time
}

type OperationStep struct {
	ID          string
	OperationID string
	StepKey     string
	Status      string
	Attempt     int
	Summary     string
	StartedAt   *time.Time
	CompletedAt *time.Time
}
```

Restate owns execution truth. These rows are API/UI projections and audit anchors.

### Billing

```go
type Subscription struct {
	ID                    string
	UserID                string
	PolarCustomerID       string
	PolarSubscriptionID   string
	Status                string
	CurrentPeriodStart    time.Time
	CurrentPeriodEnd      time.Time
}

type CreditBalance struct {
	UserID      string
	Balance     int64
	Version     int64
	UpdatedAt   time.Time
}

type CreditTransaction struct {
	ID                 string
	UserID             string
	Kind               string
	Credits            int64
	ResourceType       *string
	ResourceID         *string
	RawQuantity        *decimal.Decimal
	RawUnit            *string
	RateVersion        *string
	IdempotencyKey     string
	PolarEventID       *string
	OccurredAt         time.Time
	CreatedAt          time.Time
}
```

The Credit Balance is a projection of an immutable Credit Transaction ledger. Compute and storage debit the same pool after conversion through versioned rates.

## Database constraints

- Unique WorkOS user ID.
- Unique SSH fingerprint per user.
- Unique Profile name and Environment slug per user while active.
- Unique Profile version number and digest per Profile.
- Unique Capsule Ref ordinal per Profile Version.
- Unique Component ID per Capsule digest.
- Unique Capsule Lock digest.
- Unique `(environment_id, lock_id, capsule_digest, component_id, adapter_id)` for Materializations.
- Unique Project Binding per Environment.
- Unique State Component kind per Environment.
- Unique Auto-stop Policy per Environment.
- Unique current Runtime per Environment.
- Partial unique index for one non-terminal mutating Operation per Environment.
- Unique `(requested_by_user_id, idempotency_key)` for public mutations.
- Unique provider resource identity per provider, region, type, and provider ID.
- Unique billing idempotency key and Polar delivery key.
- Append-only Audit Event and Credit Transaction enforcement at the application boundary, backed by restricted database roles.
