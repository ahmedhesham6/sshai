output "service_arn" {
  description = "ARN of this independently deployable ECS service."
  value       = aws_ecs_service.service.id
}

output "task_definition_arn" {
  description = "ARN of the service's current task definition."
  value       = aws_ecs_task_definition.service.arn
}

output "task_role_arn" {
  description = "ARN of the service-specific application task role."
  value       = aws_iam_role.task.arn
}

output "execution_role_arn" {
  description = "ARN of the service-specific ECS execution role."
  value       = aws_iam_role.execution.arn
}

output "log_group_name" {
  description = "CloudWatch log group receiving service task output."
  value       = aws_cloudwatch_log_group.service.name
}

output "load_balancer_decision_inputs" {
  description = "Required unresolved item-11 values for the separate load-balancer/proxy configuration."
  value       = var.load_balancer
}

output "autoscaling_target" {
  description = "Optional capacity target to which a caller may attach a ratified scaling policy."
  value = var.scaling == null ? null : {
    resource_id        = one(aws_appautoscaling_target.service).resource_id
    scalable_dimension = one(aws_appautoscaling_target.service).scalable_dimension
    service_namespace  = one(aws_appautoscaling_target.service).service_namespace
  }
}
