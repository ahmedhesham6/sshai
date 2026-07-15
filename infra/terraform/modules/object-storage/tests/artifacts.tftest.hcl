mock_provider "aws" {}

variables {
  bucket_name = "sshai-development-us-east-1-artifacts"
  tags = {
    managed-by = "terraform"
    project    = "sshai"
  }
}

run "protects_immutable_artifacts" {
  command = apply

  assert {
    condition     = aws_s3_bucket.artifacts.bucket == var.bucket_name && !aws_s3_bucket.artifacts.force_destroy
    error_message = "Artifact storage must use its explicit name and preserve objects during bucket deletion."
  }

  assert {
    condition     = aws_s3_bucket_versioning.artifacts.versioning_configuration[0].status == "Enabled"
    error_message = "Artifact storage must retain object versions."
  }

  assert {
    condition     = one(one(aws_s3_bucket_server_side_encryption_configuration.artifacts.rule).apply_server_side_encryption_by_default).sse_algorithm == "AES256"
    error_message = "Artifact storage must encrypt objects at rest."
  }

  assert {
    condition     = alltrue([aws_s3_bucket_public_access_block.artifacts.block_public_acls, aws_s3_bucket_public_access_block.artifacts.block_public_policy, aws_s3_bucket_public_access_block.artifacts.ignore_public_acls, aws_s3_bucket_public_access_block.artifacts.restrict_public_buckets])
    error_message = "All S3 public access controls must be blocked."
  }

  assert {
    condition     = strcontains(aws_s3_bucket_policy.secure_transport.policy, "aws:SecureTransport")
    error_message = "Artifact storage must reject transport without TLS."
  }
}
