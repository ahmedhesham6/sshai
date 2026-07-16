output "bucket_id" {
  description = "Capsule store bucket ID."
  value       = aws_s3_bucket.capsules.id
}

output "bucket_arn" {
  description = "Capsule store bucket ARN."
  value       = aws_s3_bucket.capsules.arn
}

output "control_plane_presigning_policy_arn" {
  description = "IAM policy ARN for owner-scoped control-plane presigning."
  value       = aws_iam_policy.control_plane_presigning.arn
}

output "control_plane_presigning_policy_json" {
  description = "IAM policy JSON for owner-scoped control-plane presigning."
  value       = data.aws_iam_policy_document.control_plane_presigning.json
}
