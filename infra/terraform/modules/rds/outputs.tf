output "endpoint" {
  description = "Private PostgreSQL endpoint."
  value       = aws_db_instance.postgres.address
}

output "port" {
  description = "Private PostgreSQL port."
  value       = aws_db_instance.postgres.port
}

output "security_group_id" {
  description = "Database security group ID."
  value       = aws_security_group.postgres.id
}

output "instance_arn" {
  description = "Database ARN for least-privilege IAM policies."
  value       = aws_db_instance.postgres.arn
}
