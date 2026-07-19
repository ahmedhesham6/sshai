mock_provider "aws" {
  mock_data "aws_iam_policy_document" {
    defaults = {
      json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }
}

variables {
  bucket_name = "sshai-development-eu-central-1-capsules"
  tags = {
    managed-by = "terraform"
    project    = "sshai"
  }
}

run "protects_the_owner_scoped_capsule_store" {
  command = apply

  assert {
    condition     = aws_s3_bucket.capsules.bucket == var.bucket_name && !aws_s3_bucket.capsules.force_destroy
    error_message = "The Capsule store must use its explicit name and preserve stored objects."
  }

  assert {
    condition     = aws_s3_bucket_versioning.capsules.versioning_configuration[0].status == "Enabled"
    error_message = "The Capsule store must retain object versions."
  }

  assert {
    condition     = one(one(aws_s3_bucket_server_side_encryption_configuration.capsules.rule).apply_server_side_encryption_by_default).sse_algorithm == "AES256"
    error_message = "The Capsule store must encrypt objects at rest."
  }

  assert {
    condition     = alltrue([aws_s3_bucket_public_access_block.capsules.block_public_acls, aws_s3_bucket_public_access_block.capsules.block_public_policy, aws_s3_bucket_public_access_block.capsules.ignore_public_acls, aws_s3_bucket_public_access_block.capsules.restrict_public_buckets])
    error_message = "All Capsule store public access controls must be blocked."
  }

  assert {
    condition     = strcontains(aws_s3_bucket_policy.secure_transport.policy, "Deny") && strcontains(aws_s3_bucket_policy.secure_transport.policy, "aws:SecureTransport") && strcontains(aws_s3_bucket_policy.secure_transport.policy, "false")
    error_message = "The Capsule store must deny requests made without secure transport."
  }
}
