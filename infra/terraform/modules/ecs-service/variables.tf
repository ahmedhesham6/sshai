variable "name_prefix" {
  description = "Prefix used for the service's deployment resources."
  type        = string
}

variable "service_name" {
  description = "Service identity, such as control-plane, workflows, or ssh-proxy."
  type        = string

  validation {
    condition     = contains(["control-plane", "workflows", "ssh-proxy"], var.service_name)
    error_message = "service_name must be control-plane, workflows, or ssh-proxy."
  }
}

variable "cluster_arn" {
  description = "ARN of the ECS cluster that owns the service."
  type        = string
}

variable "container_image" {
  description = "Immutable container image reference, preferably an ECR digest."
  type        = string

  validation {
    condition     = strcontains(var.container_image, "@sha256:")
    error_message = "container_image must be pinned by sha256 digest."
  }
}

variable "container_port" {
  description = "TCP port exposed by the service container."
  type        = number

  validation {
    condition     = var.container_port >= 1 && var.container_port <= 65535
    error_message = "container_port must be a valid TCP port."
  }
}

variable "task_cpu" {
  description = "Fargate task CPU units."
  type        = number
}

variable "task_memory" {
  description = "Fargate task memory in MiB."
  type        = number
}

variable "desired_count" {
  description = "Initial desired task count before Application Auto Scaling takes ownership."
  type        = number

  validation {
    condition     = var.desired_count >= 1
    error_message = "desired_count must be at least one."
  }
}

variable "subnet_ids" {
  description = "Private service subnet IDs used by Fargate tasks."
  type        = list(string)

  validation {
    condition     = length(var.subnet_ids) >= 2
    error_message = "subnet_ids must include at least two private service subnets."
  }
}

variable "security_group_id" {
  description = "Distinct service security group ID supplied by the regional cell."
  type        = string
}

variable "load_balancer" {
  # TODO(decision-register): item 11 must settle ALB WebSocket settings,
  # keepalive, maximum connection age, and regional behavior. The service
  # module deliberately requires the decided target and grace period instead
  # of choosing those policy values.
  description = "Required load-balancer attachment values; callers own unresolved decision-register item 11."
  type = object({
    target_group_arn                  = string
    health_check_grace_period_seconds = number
    websocket_idle_timeout_seconds    = number
    client_keep_alive_seconds         = number
    maximum_connection_age_seconds    = number
  })
}

variable "scaling" {
  # TODO(decision-register): item 11 includes regional proxy scaling. Keep all
  # service capacity policy explicit at each call site until it is ratified.
  description = "Required per-service scaling policy values."
  type = object({
    minimum_tasks      = number
    maximum_tasks      = number
    target_cpu_percent = number
    scale_in_cooldown  = number
    scale_out_cooldown = number
  })

  validation {
    condition     = var.scaling.minimum_tasks >= 1 && var.scaling.maximum_tasks >= var.scaling.minimum_tasks && var.scaling.target_cpu_percent >= 1 && var.scaling.target_cpu_percent <= 100
    error_message = "scaling must have a positive ordered task range and a CPU target from 1 to 100."
  }
}

variable "environment" {
  description = "Non-secret environment variables passed to the container."
  type        = map(string)
  default     = {}
}

variable "secrets" {
  description = "Environment variable names mapped to Secrets Manager or SSM parameter ARNs."
  type        = map(string)
  default     = {}
}

variable "task_role_policy_json" {
  description = "Required least-privilege IAM policy JSON for this service's distinct task role."
  type        = string
}

variable "log_retention_days" {
  description = "CloudWatch log retention for service task output."
  type        = number
  default     = 30
}

variable "tags" {
  description = "Tags applied to service resources."
  type        = map(string)
  default     = {}
}
