# Infrastructure and regional cells

## Environments

- `local`: fake provider; local PostgreSQL and Restate server permitted for development.
- `development`: dedicated AWS account with disposable tagged resources.
- `staging`: production-shaped global plane and one regional cell.
- `production`: separate AWS account, WorkOS environment, Polar environment, RDS, Restate Cloud environment, IAM, and budgets.

Never share data-plane resources, databases, hosted endpoints, or secrets across staging and production.

## Global product plane

### ECS Fargate services

- control-plane API;
- Restate workflow endpoint;
- optional internal reconciliation/administrative endpoint if not hosted with workflows.

Each service has a distinct task role, security group, scaling policy, and deployment identity.

### RDS PostgreSQL

- encrypted storage;
- private subnets;
- no public endpoint;
- automated backups and point-in-time recovery;
- Single-AZ allowed for development/private alpha;
- Multi-AZ required before broad paid availability;
- separate application, migration, and read-only support roles.

### Object storage

Encrypted S3 buckets for:

- immutable Capsule blobs and manifests;
- Project Seed objects;
- redacted operation diagnostics when necessary;
- Packer build outputs and manifests where appropriate.

Use per-environment prefixes, server-side encryption, short-lived presigned uploads, digest verification, lifecycle policies, and blocked public access.

## Regional cell

Every enabled AWS region contains:

- one VPC;
- public subnets for load balancers and NAT;
- private Runtime subnets;
- private service subnets when global services are colocated;
- one managed NAT gateway per enabled alpha region/AZ;
- S3 gateway endpoint;
- regional ECS SSH proxy;
- EC2 launch templates and approved AMIs;
- Runtime and State Component security groups;
- logging and alarms;
- a region-specific Runtime Preset mapping and credit-rate configuration.

The private alpha enables one regional cell. Adding a region deploys the same module and test suite with new rate and capacity configuration.

### Regional Capsule store

The regional cell stores Capsules in content-addressed S3 using the OCI image-layout
format. Objects use a bucket layout under per-owner prefixes, and the control plane
mints short-lived presigned URLs scoped to the owner prefix for Capsule pulls and pushes.
The S3 gateway endpoint keeps Capsule traffic on AWS networking and reduces NAT cost.

Zot on ECS Fargate is deferred to the sharing milestone.

## Runtime resources

Per Environment/current Runtime:

- one EC2 instance;
- one replaceable encrypted root EBS volume;
- one persistent encrypted gp3 data EBS volume;
- security group accepting SSH only from the regional proxy;
- Environment-scoped guest credential;
- provider ownership tags.

No Runtime public IPv4, Elastic IP, public DNS record, user data containing durable secrets, or instance role with platform mutation permissions.

## Runtime Presets

Product contracts expose stable logical names and resource quantities. Provider instance-family mappings are regional configuration, not domain constants.

Initial shape recommendation, pending benchmarking:

- `small`: 2 vCPU / 4 GiB;
- `standard`: 4 vCPU / 8 GiB;
- `large`: 8 vCPU / 16 GiB.

Mappings must use EBS-backed x86-64 types available in the Environment's fixed AZ and compatible with stop/start resize if resizing is added.

## Image pipeline

Packer builds a versioned Ubuntu 24.04 x86-64 AMI per enabled region.

Build gates:

- package sources pinned or verified;
- guest supervisor binary checksum;
- OpenSSH hardening test;
- Docker/Compose smoke test;
- data-volume mount test;
- reboot/readiness test;
- vulnerability scan and SBOM;
- image manifest stored with source revision;
- promotion from development to staging to production by immutable AMI ID.

Cloud-init performs only Environment enrollment, authorized-key configuration, guest credential bootstrap, and volume discovery. It is not the full machine build system.

## Terraform structure

```text
infra/terraform/
  modules/
    global-networking/
    regional-cell/
    ecs-service/
    rds/
    object-storage/
    iam/
    image-pipeline/
    observability/
  environments/
    development/
    staging/
    production/
```

Platform Terraform creates shared infrastructure only. Per-Environment resources are created through the AWS provider adapter and Restate workflows, not one Terraform state per Environment.

## Secrets

Use AWS Secrets Manager or SSM Parameter Store for platform credentials and ECS secret injection. WorkOS, Polar, Restate Cloud, database, and signing material are never stored in repository files or Terraform state values when an indirection is available.

## Budget controls

- private-alpha account allowlist;
- user Environment quota;
- maximum Runtime Preset and storage allocation;
- regional capacity and spend limits;
- provider budget alarms;
- kill switch blocking new create/start operations without affecting stopped storage;
- leak detector for untracked instances, volumes, and security groups.
