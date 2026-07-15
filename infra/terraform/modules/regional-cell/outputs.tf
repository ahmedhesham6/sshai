output "vpc_id" {
  description = "Regional cell VPC ID."
  value       = aws_vpc.cell.id
}

output "public_subnet_ids" {
  description = "Public subnet IDs ordered by input availability zone."
  value       = [for index in ["0", "1"] : aws_subnet.public[index].id]
}

output "service_subnet_ids" {
  description = "Private platform service subnet IDs ordered by input availability zone."
  value       = [for index in ["0", "1"] : aws_subnet.service[index].id]
}

output "runtime_subnet_ids" {
  description = "Private Runtime subnet IDs ordered by input availability zone."
  value       = [for index in ["0", "1"] : aws_subnet.runtime[index].id]
}

output "nat_gateway_id" {
  description = "Single managed NAT gateway used by the private-alpha cell."
  value       = aws_nat_gateway.egress.id
}

output "load_balancer_security_group_id" {
  description = "Security group for the regional public load balancer."
  value       = aws_security_group.load_balancer.id
}

output "ecs_service_security_group_ids" {
  description = "Distinct security groups for each regional ECS service."
  value       = local.ecs_service_security_groups
}

output "runtime_security_group_id" {
  description = "Security group for private Runtime instances."
  value       = aws_security_group.runtime.id
}
