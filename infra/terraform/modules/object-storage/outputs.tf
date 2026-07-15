output "bucket_id" {
  description = "Artifact bucket ID."
  value       = aws_s3_bucket.artifacts.id
}

output "bucket_arn" {
  description = "Artifact bucket ARN for least-privilege IAM policies."
  value       = aws_s3_bucket.artifacts.arn
}
