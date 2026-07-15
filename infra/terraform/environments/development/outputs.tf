output "vpc_id" {
  description = "Private-alpha regional cell VPC ID."
  value       = module.regional_cell.vpc_id
}

output "public_subnet_ids" {
  description = "Public load-balancer and NAT subnet IDs."
  value       = module.regional_cell.public_subnet_ids
}

output "service_subnet_ids" {
  description = "Private platform service and database subnet IDs."
  value       = module.regional_cell.service_subnet_ids
}

output "runtime_subnet_ids" {
  description = "Private Runtime subnet IDs."
  value       = module.regional_cell.runtime_subnet_ids
}

output "nat_gateway_id" {
  description = "Single private-alpha managed NAT gateway ID."
  value       = module.regional_cell.nat_gateway_id
}

output "ecs_service_security_group_ids" {
  description = "Security groups for the control-plane, workflows, and regional SSH proxy tasks."
  value       = module.regional_cell.ecs_service_security_group_ids
}

output "runtime_security_group_id" {
  description = "Security group enforcing proxy-only Runtime SSH ingress."
  value       = module.regional_cell.runtime_security_group_id
}

output "artifact_bucket_id" {
  description = "Encrypted, versioned artifact bucket ID."
  value       = module.artifact_storage.bucket_id
}

output "artifact_bucket_arn" {
  description = "Artifact bucket ARN for least-privilege IAM policies."
  value       = module.artifact_storage.bucket_arn
}

output "database_endpoint" {
  description = "Private PostgreSQL endpoint."
  value       = module.postgres.endpoint
}

output "database_port" {
  description = "Private PostgreSQL port."
  value       = module.postgres.port
}

output "database_security_group_id" {
  description = "Private PostgreSQL security group ID."
  value       = module.postgres.security_group_id
}
