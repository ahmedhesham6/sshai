output "codebuild_project_arn" {
  description = "ARN of the weekly Runtime AMI build project."
  value       = aws_codebuild_project.runtime_image.arn
}

output "manifest_bucket_id" {
  description = "Bucket containing versioned image manifests keyed by source revision."
  value       = aws_s3_bucket.artifacts.id
}

output "manifest_bucket_arn" {
  description = "ARN of the immutable image-manifest bucket."
  value       = aws_s3_bucket.artifacts.arn
}

output "weekly_schedule_arn" {
  description = "ARN of the weekly image build schedule."
  value       = aws_cloudwatch_event_rule.weekly.arn
}
