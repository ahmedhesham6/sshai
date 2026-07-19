output "arn" {
  description = "ARN of the shared ECS Fargate cluster."
  value       = aws_ecs_cluster.platform.arn
}

output "name" {
  description = "Name of the shared ECS Fargate cluster."
  value       = aws_ecs_cluster.platform.name
}
