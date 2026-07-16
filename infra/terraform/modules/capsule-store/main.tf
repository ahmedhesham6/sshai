resource "aws_s3_bucket" "capsules" {
  bucket        = var.bucket_name
  force_destroy = false

  tags = merge(var.tags, { Name = var.bucket_name })
}

resource "aws_s3_bucket_versioning" "capsules" {
  bucket = aws_s3_bucket.capsules.id

  # Keep versioning enabled for audit and recovery. The control plane's
  # owner-scoped PUT grants also sign If-None-Match: *, so a retry cannot
  # overwrite an existing immutable Capsule object.
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "capsules" {
  bucket = aws_s3_bucket.capsules.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_public_access_block" "capsules" {
  bucket = aws_s3_bucket.capsules.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_lifecycle_configuration" "capsules" {
  bucket = aws_s3_bucket.capsules.id

  rule {
    id     = "owner-capsule-version-retention"
    status = "Enabled"

    filter {
      prefix = "owner/"
    }

    noncurrent_version_expiration {
      noncurrent_days = 30
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }
}

data "aws_iam_policy_document" "control_plane_presigning" {
  statement {
    sid    = "OwnerScopedCapsuleObjects"
    effect = "Allow"

    actions = [
      "s3:GetObject",
      "s3:PutObject",
    ]

    resources = ["${aws_s3_bucket.capsules.arn}/owner/*"]
  }
}

resource "aws_iam_policy" "control_plane_presigning" {
  name        = "${var.bucket_name}-control-plane-presigning"
  description = "Owner-prefix-only Capsule object access for control-plane presigning."
  policy      = data.aws_iam_policy_document.control_plane_presigning.json
  tags        = var.tags
}

resource "aws_s3_bucket_policy" "secure_transport" {
  bucket = aws_s3_bucket.capsules.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "DenyInsecureTransport"
      Effect    = "Deny"
      Principal = "*"
      Action    = "s3:*"
      Resource = [
        aws_s3_bucket.capsules.arn,
        "${aws_s3_bucket.capsules.arn}/*",
      ]
      Condition = {
        Bool = { "aws:SecureTransport" = "false" }
      }
    }]
  })

  depends_on = [aws_s3_bucket_public_access_block.capsules]
}
