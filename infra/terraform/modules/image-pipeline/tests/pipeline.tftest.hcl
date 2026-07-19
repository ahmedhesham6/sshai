mock_provider "aws" {
  mock_resource "aws_iam_role" {
    defaults = {
      arn = "arn:aws:iam::123456789012:role/mock-image-pipeline-role"
    }
  }

  mock_resource "aws_codebuild_project" {
    defaults = {
      arn = "arn:aws:codebuild:eu-central-1:123456789012:project/mock-runtime-image"
    }
  }
}

variables {
  name_prefix           = "sshai-development-eu-central-1"
  artifact_bucket_name  = "sshai-development-eu-central-1-image-artifacts"
  source_repository_url = "https://github.com/ahmedhesham6/sshai.git"
  build_vpc_id          = "vpc-0123456789abcdef0"
  build_subnet_id       = "subnet-0123456789abcdef0"
  guest_binary_source   = "dist/sshai-guest"
  ami_kms_key_arn       = "arn:aws:kms:eu-central-1:123456789012:key/00000000-0000-0000-0000-000000000000"
  tags = {
    managed-by = "terraform"
    project    = "sshai"
  }
}

run "creates_a_weekly_verified_image_pipeline" {
  command = apply

  assert {
    condition     = aws_cloudwatch_event_rule.weekly.schedule_expression == "rate(7 days)"
    error_message = "The Runtime AMI build must run weekly."
  }

  assert {
    condition     = aws_codebuild_project.runtime_image.source[0].type == "GITHUB" && aws_codebuild_project.runtime_image.source_version == "main"
    error_message = "The build must check out the declared source ref and let CodeBuild resolve its immutable revision."
  }

  assert {
    condition     = strcontains(aws_codebuild_project.runtime_image.source[0].buildspec, "sha256sum --check --strict") && strcontains(aws_codebuild_project.runtime_image.source[0].buildspec, "guest_binary_source=$GUEST_BINARY_SOURCE") && strcontains(aws_codebuild_project.runtime_image.source[0].buildspec, "packer build")
    error_message = "The build must verify Packer and supply the guest binary before producing the AMI."
  }

  assert {
    condition     = aws_s3_bucket_versioning.artifacts.versioning_configuration[0].status == "Enabled"
    error_message = "Image manifests must be versioned."
  }

  assert {
    condition     = one(one(aws_s3_bucket_server_side_encryption_configuration.artifacts.rule).apply_server_side_encryption_by_default).sse_algorithm == "AES256"
    error_message = "Image manifests must be encrypted at rest."
  }

  assert {
    condition = (
      one([for rule in aws_s3_bucket_lifecycle_configuration.artifacts.rule : rule if rule.id == "manifest-version-retention"]).noncurrent_version_expiration[0].noncurrent_days == var.manifest_noncurrent_version_expiration_days &&
      one([for rule in aws_s3_bucket_lifecycle_configuration.artifacts.rule : rule if rule.id == "abort-incomplete-uploads"]).abort_incomplete_multipart_upload[0].days_after_initiation == var.multipart_abort_days
    )
    error_message = "Image manifest lifecycle must expire noncurrent versions and abort incomplete uploads."
  }

  assert {
    condition     = alltrue([aws_s3_bucket_public_access_block.artifacts.block_public_acls, aws_s3_bucket_public_access_block.artifacts.block_public_policy, aws_s3_bucket_public_access_block.artifacts.ignore_public_acls, aws_s3_bucket_public_access_block.artifacts.restrict_public_buckets])
    error_message = "The image artifact bucket must block all public access."
  }

  assert {
    condition     = strcontains(aws_iam_role_policy.scheduler.policy, "codebuild:StartBuild")
    error_message = "The scheduler role must be limited to starting the image build."
  }

  assert {
    condition = (
      one([for statement in jsondecode(aws_iam_role_policy.build.policy).Statement : statement if statement.Sid == "CopyTaggedEncryptedAmi"]).Condition.StringEquals["aws:RequestTag/image-pipeline"] == var.name_prefix &&
      one([for statement in jsondecode(aws_iam_role_policy.build.policy).Statement : statement if statement.Sid == "MutateOnlyOwnedBuildResources"]).Condition.StringEquals["ec2:ResourceTag/image-pipeline"] == var.name_prefix &&
      one([for statement in jsondecode(aws_iam_role_policy.build.policy).Statement : statement if statement.Sid == "UseAmiEncryptionKey"]).Resource == var.ami_kms_key_arn
    )
    error_message = "The build role must permit encrypted AMI copying while restricting mutations to pipeline-owned resources and the declared KMS key."
  }
}

run "omits_kms_permissions_without_a_customer_key" {
  command = plan

  variables {
    ami_kms_key_arn = null
  }

  assert {
    condition = length([
      for statement in jsondecode(aws_iam_role_policy.build.policy).Statement : statement
      if startswith(statement.Sid, "UseAmiEncryptionKey") || startswith(statement.Sid, "GrantAmiEncryptionKey")
    ]) == 0
    error_message = "The build role must not gain KMS permissions when no customer-managed AMI key is declared."
  }
}
