# Domain model

The model separates durable user intent, persistent filesystem state, temporary compute, durable execution, and billing. Provider identifiers never define product identity.

## Aggregate relationships

```text
User
├── Profiles
│   └── Profile Versions
│       └── Profile Artifacts
├── SSH Keys
├── Subscription
│   ├── Credit Balance
│   └── Credit Transactions
└── Environments
    ├── Project Binding
    ├── Project Spec
    ├── Project Seed
    ├── pinned Profile Version
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

### Profile and ProfileVersion

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
	CreatedAt        time.Time
}
```

Invariants:

- `(owner_user_id, slug)` is unique for non-archived Profiles.
- Profile Versions are immutable.
- `parent_version_id` creates a linear history for a Profile.
- Publication uses an expected head version; a stale writer must refresh or fork.
- A fresh CLI installation may read an existing Profile, fork it, or create another. There is no Device entity.

### ProfileArtifact

```go
type ProfileArtifact struct {
	ID               string
	ProfileVersionID string
	Kind             ArtifactKind
	SourceLocator    string
	SourceDigest     string
	ContentDigest    string
	Sensitivity      Sensitivity
	Trust            TrustClass
	ContainsExecutable bool
	Metadata         json.RawMessage
}
```

Artifact content belongs in encrypted object storage by digest. PostgreSQL stores intent and metadata.

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

Invariants:

- `(owner_user_id, slug)` is unique for non-deleted Environments.
- Region is immutable in the MVP.
- The primary Project Binding is immutable after creation except for an explicit future relink operation.
- An Environment has zero or one current Runtime.
- An Environment has at most one active mutating Operation.
- An Environment pins exactly one immutable Profile Version.

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
	ID                  string
	EnvironmentID       string
	ProfileArtifactID   string
	Mode                MaterializationMode
	Adapter             string
	AdapterVersion      string
	Target              string
	Selector            *string
	LastAppliedDigest   *string
	ObservedDigest      *string
	Status              string
	AppliedAt           *time.Time
}
```

Modes:

- `managed`: updates use three-way comparison and remote drift blocks overwrite.
- `seeded`: created once, then owned entirely by the Environment.
- `referenced`: resolved from another system at use time.

### CredentialRequirement and CredentialBinding

```go
type CredentialRequirement struct {
	ID               string
	ProfileArtifactID *string
	ProjectSpecID     *string
	Kind             string
	Scope            string
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

Profile content declares requirements and secret references, never secret values or copied authentication caches.

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
- Unique Project Binding per Environment.
- Unique State Component kind per Environment.
- Unique Auto-stop Policy per Environment.
- Unique current Runtime per Environment.
- Partial unique index for one non-terminal mutating Operation per Environment.
- Unique `(requested_by_user_id, idempotency_key)` for public mutations.
- Unique provider resource identity per provider, region, type, and provider ID.
- Unique billing idempotency key and Polar delivery key.
- Append-only Audit Event and Credit Transaction enforcement at the application boundary, backed by restricted database roles.
