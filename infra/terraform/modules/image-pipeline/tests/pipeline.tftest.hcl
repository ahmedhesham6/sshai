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
    condition     = strcontains(aws_codebuild_project.runtime_image.source[0].buildspec, "sha256sum --check --strict") && strcontains(aws_codebuild_project.runtime_image.source[0].buildspec, "packer build")
    error_message = "The build must verify Packer before producing the AMI."
  }

  assert {
    condition     = aws_s3_bucket_versioning.artifacts.versioning_configuration[0].status == "Enabled"
    error_message = "Image manifests must be versioned."
  }

  assert {
    condition     = alltrue([aws_s3_bucket_public_access_block.artifacts.block_public_acls, aws_s3_bucket_public_access_block.artifacts.block_public_policy, aws_s3_bucket_public_access_block.artifacts.ignore_public_acls, aws_s3_bucket_public_access_block.artifacts.restrict_public_buckets])
    error_message = "The image artifact bucket must block all public access."
  }

  assert {
    condition     = strcontains(aws_iam_role_policy.scheduler.policy, "codebuild:StartBuild")
    error_message = "The scheduler role must be limited to starting the image build."
  }
}
