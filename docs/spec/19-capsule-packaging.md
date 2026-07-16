# Capsule packaging

## OCI artifact layout

A Capsule is an immutable, digest-addressed OCI artifact. Its artifact type is
`application/vnd.devm.capsule.v1`.

```text
Capsule OCI artifact
├── config blob: capsule manifest
│   ├── schemaVersion
│   ├── name
│   ├── components (including trust class)
│   └── requirements
└── layers: one layer per Component
    └── canonical file index: path → digest, mode
```

The config blob is the Capsule manifest, including each Component's trust class. Each
Component has one layer. Layer media
types are type-specific, for example
`application/vnd.devm.capsule.skill.v1.tar+gzip`. Each layer carries these
annotations:

| Annotation | Value |
|---|---|
| `devm.component.type` | Component type |
| `devm.component.id` | Stable Component id |
| `devm.component.scope` | `user` or `project` |
| `devm.component.trust` | Component trust class |

Credential Requirements contain names or references only. Credential values never
enter Capsule layers or registry metadata.

## Change detection

Capsule changes are detected at four levels:

```text
package digest
  → Component layer digest
  → file digest
  → rendered native output digest
```

The package digest detects Capsule changes. The Component layer digest detects a
Component change. The file digest detects file-level changes from the canonical index.
The rendered native output digest detects adapter output changes.

Layers are pulled concurrently; cold-pull latency is bounded by the `time_to_first_ssh`
SLO.

## Deterministic packaging

Packaging uses:

- a sorted tree walk;
- pinned mtime using the `SOURCE_DATE_EPOCH` convention;
- uid and gid `0`;
- a cleared gzip header;
- a pinned Go toolchain through the `go.mod` `toolchain` directive, enforced in CI;
- stripped xattrs and ACLs (structurally guaranteed by the USTAR tar format).

Determinism is a CI gate: the same tree must produce the same digest on two machines.

## Materialization cache key

Materialization identity remains:

```text
(lock, capsule digest, component id, adapter)
```

The effective cache key is:

```text
component digest
+ adapter id
+ adapter version
+ target agent version
+ scope
+ non-secret overrides digest
+ secret version identifiers
```

Resolved secret values never enter the key or any record.

## Registry strategy

The packaging library is oras-go v2, version `2.6.2` or newer.

For MVP, the capsule store is content-addressed S3 using the OCI image-layout format.
The control plane mints short-lived presigned GETs for pulls, scoped per authenticated
owner prefix, and owner-scoped short-lived presigned grants for uploads. The oras-go
client uses its `Target` abstraction so the client is identical across OCI image-layout
and remote registry backends.

The implementation never depends on the OCI 1.1 Referrers API. The remote-registry
backend uses the ORAS tag-fallback because registry support is fragmented: ECR has a
bug, GHCR is inconsistent, and `registry:2` lacks the API.

The hosted OCI Distribution registry (Zot), external registries such as GHCR and ECR,
and signing are deferred to the sharing milestone.

## Blob caching

The guest caches pulled blobs in the `cache` State Component. A warm Environment starts
without capsule store availability.

## Signing roadmap

Signing is deferred to the sharing milestone. The plan is cosign keyless signing;
Notation is documented as a bring-your-own-PKI alternative. Provenance is not review;
signing never replaces the consent boundary. Until signing ships, Environments may
reference only Capsules owned by the authenticated user; the control plane enforces this
constraint. The ORAS fallback-tag overwrite surface must be resolved before signatures
attach through it.
