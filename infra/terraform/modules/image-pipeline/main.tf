locals {
  ownership_request_tags = {
    "aws:RequestTag/image-pipeline" = var.name_prefix
    "aws:RequestTag/managed-by"     = "packer"
    "aws:RequestTag/project"        = "sshai"
  }
  ownership_resource_tags = {
    "ec2:ResourceTag/image-pipeline" = var.name_prefix
    "ec2:ResourceTag/managed-by"     = "packer"
    "ec2:ResourceTag/project"        = "sshai"
  }
  kms_policy_statements = jsondecode(var.ami_kms_key_arn == null ? "[]" : jsonencode([
    {
      Sid    = "UseAmiEncryptionKey"
      Effect = "Allow"
      Action = [
        "kms:Decrypt",
        "kms:DescribeKey",
        "kms:GenerateDataKeyWithoutPlaintext",
        "kms:ReEncryptFrom",
        "kms:ReEncryptTo",
      ]
      Resource = var.ami_kms_key_arn
    },
    {
      Sid      = "GrantAmiEncryptionKeyToAws"
      Effect   = "Allow"
      Action   = "kms:CreateGrant"
      Resource = var.ami_kms_key_arn
      Condition = {
        Bool = { "kms:GrantIsForAWSResource" = "true" }
      }
    },
  ]))

  # Implementation choice: spec 13 does not prescribe an image runner. A
  # scheduled CodeBuild job is the smallest AWS-native runner for the existing
  # Packer template and avoids a permanently running build host.
  buildspec = yamlencode({
    version = 0.2
    phases = {
      install = {
        commands = [
          "curl -fsSLo /tmp/packer.zip https://releases.hashicorp.com/packer/$PACKER_VERSION/packer_$${PACKER_VERSION}_linux_amd64.zip",
          "echo \"$PACKER_SHA256  /tmp/packer.zip\" | sha256sum --check --strict",
          "unzip -q /tmp/packer.zip -d /usr/local/bin",
          "packer version",
        ]
      }
      build = {
        commands = [
          "packer init images/packer/runtime.pkr.hcl",
          "packer validate -var \"source_revision=$CODEBUILD_RESOLVED_SOURCE_VERSION\" -var \"build_vpc_id=$BUILD_VPC_ID\" -var \"build_subnet_id=$BUILD_SUBNET_ID\" -var \"aws_region=$PACKER_REGION\" -var \"pipeline_id=$PACKER_PIPELINE_ID\" -var \"guest_binary_source=$GUEST_BINARY_SOURCE\" -var \"ami_kms_key_id=$AMI_KMS_KEY_ARN\" images/packer/runtime.pkr.hcl",
          "packer build -var \"source_revision=$CODEBUILD_RESOLVED_SOURCE_VERSION\" -var \"build_vpc_id=$BUILD_VPC_ID\" -var \"build_subnet_id=$BUILD_SUBNET_ID\" -var \"aws_region=$PACKER_REGION\" -var \"pipeline_id=$PACKER_PIPELINE_ID\" -var \"guest_binary_source=$GUEST_BINARY_SOURCE\" -var \"ami_kms_key_id=$AMI_KMS_KEY_ARN\" images/packer/runtime.pkr.hcl",
        ]
      }
      post_build = {
        commands = [
          "aws s3 cp images/packer/manifest.json s3://$MANIFEST_BUCKET/manifests/$CODEBUILD_RESOLVED_SOURCE_VERSION.json --sse AES256 --no-progress",
        ]
      }
    }
  })
}

resource "aws_s3_bucket" "artifacts" {
  bucket        = var.artifact_bucket_name
  force_destroy = false

  tags = merge(var.tags, { Name = var.artifact_bucket_name })
}

resource "aws_s3_bucket_versioning" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_public_access_block" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_lifecycle_configuration" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id

  rule {
    id     = "manifest-version-retention"
    status = "Enabled"

    filter {
      prefix = "manifests/"
    }

    noncurrent_version_expiration {
      noncurrent_days = var.manifest_noncurrent_version_expiration_days
    }
  }

  rule {
    id     = "abort-incomplete-uploads"
    status = "Enabled"

    filter {}

    abort_incomplete_multipart_upload {
      days_after_initiation = var.multipart_abort_days
    }
  }

  depends_on = [aws_s3_bucket_versioning.artifacts]
}

resource "aws_s3_bucket_policy" "secure_transport" {
  bucket = aws_s3_bucket.artifacts.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "DenyInsecureTransport"
      Effect    = "Deny"
      Principal = "*"
      Action    = "s3:*"
      Resource = [
        aws_s3_bucket.artifacts.arn,
        "${aws_s3_bucket.artifacts.arn}/*",
      ]
      Condition = {
        Bool = { "aws:SecureTransport" = "false" }
      }
    }]
  })

  depends_on = [aws_s3_bucket_public_access_block.artifacts]
}

resource "aws_cloudwatch_log_group" "build" {
  name              = "/aws/codebuild/${var.name_prefix}-runtime-image"
  retention_in_days = 30

  tags = var.tags
}

resource "aws_iam_role" "build" {
  name = "${var.name_prefix}-runtime-image-build"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        Service = "codebuild.amazonaws.com"
      }
      Action = "sts:AssumeRole"
    }]
  })

  tags = var.tags
}

resource "aws_iam_role_policy" "build" {
  name = "runtime-image-build"
  role = aws_iam_role.build.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Sid    = "BuildLogs"
        Effect = "Allow"
        Action = [
          "logs:CreateLogStream",
          "logs:PutLogEvents",
        ]
        Resource = "${aws_cloudwatch_log_group.build.arn}:*"
      },
      {
        Sid      = "StoreManifest"
        Effect   = "Allow"
        Action   = ["s3:GetBucketLocation", "s3:ListBucket"]
        Resource = aws_s3_bucket.artifacts.arn
      },
      {
        Sid      = "WriteManifest"
        Effect   = "Allow"
        Action   = ["s3:PutObject"]
        Resource = "${aws_s3_bucket.artifacts.arn}/manifests/*"
      },
      {
        Sid    = "DescribeBuildResources"
        Effect = "Allow"
        Action = [
          "ec2:DescribeAccountAttributes",
          "ec2:DescribeAvailabilityZones",
          "ec2:DescribeImageAttribute",
          "ec2:DescribeImages",
          "ec2:DescribeInstances",
          "ec2:DescribeInstanceAttribute",
          "ec2:DescribeInstanceStatus",
          "ec2:DescribeKeyPairs",
          "ec2:DescribeRegions",
          "ec2:DescribeSecurityGroups",
          "ec2:DescribeSnapshots",
          "ec2:DescribeSubnets",
          "ec2:DescribeTags",
          "ec2:DescribeVolumes",
          "ec2:DescribeVolumeStatus",
          "ec2:DescribeVpcs",
        ]
        Resource = "*"
      },
      {
        Sid    = "CreateTaggedBuildResources"
        Effect = "Allow"
        Action = [
          "ec2:CreateImage",
          "ec2:CreateKeyPair",
          "ec2:CreateSecurityGroup",
          "ec2:CreateSnapshot",
          "ec2:CreateVolume",
          "ec2:RunInstances",
        ]
        Resource = "*"
        Condition = {
          StringEquals = local.ownership_request_tags
        }
      },
      {
        Sid      = "CopyTaggedEncryptedAmi"
        Effect   = "Allow"
        Action   = "ec2:CopyImage"
        Resource = "*"
        Condition = {
          StringEquals = local.ownership_request_tags
        }
      },
      {
        Sid      = "TagResourcesDuringOwnedCreation"
        Effect   = "Allow"
        Action   = "ec2:CreateTags"
        Resource = "*"
        Condition = {
          StringEquals = merge(local.ownership_request_tags, {
            "ec2:CreateAction" = [
              "CreateImage",
              "CreateKeyPair",
              "CopyImage",
              "CreateSecurityGroup",
              "CreateSnapshot",
              "CreateVolume",
              "RunInstances",
            ]
          })
        }
      },
      {
        Sid      = "RetagOwnedBuildResources"
        Effect   = "Allow"
        Action   = "ec2:CreateTags"
        Resource = "*"
        Condition = {
          StringEquals = local.ownership_resource_tags
        }
      },
      {
        Sid    = "MutateOnlyOwnedBuildResources"
        Effect = "Allow"
        Action = [
          "ec2:AttachVolume",
          "ec2:AuthorizeSecurityGroupIngress",
          "ec2:DeleteKeyPair",
          "ec2:DeleteSecurityGroup",
          "ec2:DeleteSnapshot",
          "ec2:DeleteVolume",
          "ec2:DeregisterImage",
          "ec2:DetachVolume",
          "ec2:ModifyImageAttribute",
          "ec2:ModifyInstanceAttribute",
          "ec2:ModifySnapshotAttribute",
          "ec2:RevokeSecurityGroupIngress",
          "ec2:StopInstances",
          "ec2:TerminateInstances",
        ]
        Resource = "*"
        Condition = {
          StringEquals = local.ownership_resource_tags
        }
      },
    ], local.kms_policy_statements)
  })
}

resource "aws_codebuild_project" "runtime_image" {
  name          = "${var.name_prefix}-runtime-image"
  description   = "Weekly pinned Runtime AMI build for ${var.aws_region}"
  service_role  = aws_iam_role.build.arn
  build_timeout = 120

  artifacts {
    type = "NO_ARTIFACTS"
  }

  environment {
    compute_type = "BUILD_GENERAL1_SMALL"
    image        = "aws/codebuild/standard:7.0"
    type         = "LINUX_CONTAINER"

    environment_variable {
      name  = "PACKER_REGION"
      value = var.aws_region
    }

    environment_variable {
      name  = "BUILD_SUBNET_ID"
      value = var.build_subnet_id
    }

    environment_variable {
      name  = "BUILD_VPC_ID"
      value = var.build_vpc_id
    }

    environment_variable {
      name  = "PACKER_PIPELINE_ID"
      value = var.name_prefix
    }

    environment_variable {
      name  = "GUEST_BINARY_SOURCE"
      value = var.guest_binary_source
    }

    environment_variable {
      name  = "AMI_KMS_KEY_ARN"
      value = var.ami_kms_key_arn == null ? "" : var.ami_kms_key_arn
    }

    environment_variable {
      name  = "MANIFEST_BUCKET"
      value = aws_s3_bucket.artifacts.id
    }

    environment_variable {
      name  = "PACKER_SHA256"
      value = var.packer_linux_amd64_sha256
    }

    environment_variable {
      name  = "PACKER_VERSION"
      value = var.packer_version
    }
  }

  logs_config {
    cloudwatch_logs {
      group_name = aws_cloudwatch_log_group.build.name
    }
  }

  source {
    type            = "GITHUB"
    location        = var.source_repository_url
    git_clone_depth = 1
    buildspec       = local.buildspec
  }

  source_version = var.source_ref
  tags           = var.tags
}

resource "aws_cloudwatch_event_rule" "weekly" {
  name                = "${var.name_prefix}-runtime-image-weekly"
  description         = "Start the weekly Runtime AMI build."
  schedule_expression = var.schedule_expression

  tags = var.tags
}

resource "aws_iam_role" "scheduler" {
  name = "${var.name_prefix}-runtime-image-scheduler"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        Service = "events.amazonaws.com"
      }
      Action = "sts:AssumeRole"
    }]
  })

  tags = var.tags
}

resource "aws_iam_role_policy" "scheduler" {
  name = "start-runtime-image-build"
  role = aws_iam_role.scheduler.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = "codebuild:StartBuild"
      Resource = aws_codebuild_project.runtime_image.arn
    }]
  })
}

resource "aws_cloudwatch_event_target" "weekly" {
  rule     = aws_cloudwatch_event_rule.weekly.name
  arn      = aws_codebuild_project.runtime_image.arn
  role_arn = aws_iam_role.scheduler.arn
}
