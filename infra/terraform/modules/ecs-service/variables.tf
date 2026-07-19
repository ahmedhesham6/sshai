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
    condition     = can(regex("@sha256:[0-9a-f]{64}$", var.container_image))
    error_message = "container_image must end in a lowercase 64-hex sha256 digest."
  }
}

variable "container_repository_arn" {
  description = "ARN of the one ECR repository the execution role may pull from."
  type        = string

  validation {
    condition     = can(regex("^arn:[^:]+:ecr:[^:]+:[0-9]{12}:repository/.+$", var.container_repository_arn))
    error_message = "container_repository_arn must be an ECR repository ARN."
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
  # TODO(decision-register-11): the module may create an optional capacity
  # target, but it deliberately creates no scaling policy or signal. Callers
  # may attach a policy to the exported target only after the relevant policy
  # is explicitly ratified.
  description = "Optional autoscaling capacity bounds; no scaling signal or policy is selected by this module."
  type = object({
    minimum_tasks = number
    maximum_tasks = number
  })
  default  = null
  nullable = true

  validation {
    condition     = var.scaling == null || var.scaling.minimum_tasks >= 1 && var.scaling.maximum_tasks >= var.scaling.minimum_tasks
    error_message = "scaling must be null or have a positive ordered task range."
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
