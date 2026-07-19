locals {
  resource_name                  = "${var.name_prefix}-${var.service_name}"
  container_repository_arn_parts = split(":", var.container_repository_arn)
  container_repository_name      = trimprefix(local.container_repository_arn_parts[5], "repository/")
  container_repository_image_prefix = format(
    "%s.dkr.ecr.%s.amazonaws.com/%s@sha256:",
    local.container_repository_arn_parts[4],
    local.container_repository_arn_parts[3],
    local.container_repository_name,
  )
  container_environment = [
    for name in sort(keys(var.environment)) : {
      name  = name
      value = var.environment[name]
    }
  ]
  container_secrets = [
    for name in sort(keys(var.secrets)) : {
      name      = name
      valueFrom = var.secrets[name]
    }
  ]
}

resource "aws_cloudwatch_log_group" "service" {
  name              = "/ecs/${local.resource_name}"
  retention_in_days = var.log_retention_days

  tags = var.tags
}

resource "aws_iam_role" "execution" {
  name = "${local.resource_name}-execution"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        Service = "ecs-tasks.amazonaws.com"
      }
      Action = "sts:AssumeRole"
    }]
  })

  tags = var.tags
}

resource "aws_iam_role_policy" "execution" {
  name = "${var.service_name}-execution"
  role = aws_iam_role.execution.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "AuthenticateToEcr"
        Effect   = "Allow"
        Action   = "ecr:GetAuthorizationToken"
        Resource = "*"
      },
      {
        Sid    = "PullServiceImage"
        Effect = "Allow"
        Action = [
          "ecr:BatchCheckLayerAvailability",
          "ecr:BatchGetImage",
          "ecr:GetDownloadUrlForLayer",
        ]
        Resource = var.container_repository_arn
      },
      {
        Sid    = "WriteServiceLogs"
        Effect = "Allow"
        Action = [
          "logs:CreateLogStream",
          "logs:PutLogEvents",
        ]
        Resource = "${aws_cloudwatch_log_group.service.arn}:*"
      },
    ]
  })
}

resource "aws_iam_role_policy" "execution_secrets" {
  count = length(var.secrets) == 0 ? 0 : 1

  name = "${var.service_name}-secret-injection"
  role = aws_iam_role.execution.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "secretsmanager:GetSecretValue",
        "ssm:GetParameters",
      ]
      Resource = values(var.secrets)
    }]
  })
}

resource "aws_iam_role" "task" {
  name = "${local.resource_name}-task"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        Service = "ecs-tasks.amazonaws.com"
      }
      Action = "sts:AssumeRole"
    }]
  })

  tags = var.tags
}

resource "aws_iam_role_policy" "task" {
  name   = "${var.service_name}-task"
  role   = aws_iam_role.task.id
  policy = var.task_role_policy_json
}

resource "aws_ecs_task_definition" "service" {
  family                   = local.resource_name
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = tostring(var.task_cpu)
  memory                   = tostring(var.task_memory)
  execution_role_arn       = aws_iam_role.execution.arn
  task_role_arn            = aws_iam_role.task.arn

  runtime_platform {
    cpu_architecture        = "X86_64"
    operating_system_family = "LINUX"
  }

  container_definitions = jsonencode([{
    name      = var.service_name
    image     = var.container_image
    essential = true
    portMappings = [{
      name          = "http"
      containerPort = var.container_port
      hostPort      = var.container_port
      protocol      = "tcp"
    }]
    environment = local.container_environment
    secrets     = local.container_secrets
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.service.name
        awslogs-region        = data.aws_region.current.region
        awslogs-stream-prefix = var.service_name
      }
    }
  }])

  lifecycle {
    precondition {
      condition     = startswith(var.container_image, local.container_repository_image_prefix)
      error_message = "container_image must reference the repository granted to the execution role."
    }
  }

  tags = var.tags
}

data "aws_region" "current" {}

resource "aws_ecs_service" "service" {
  name                               = local.resource_name
  cluster                            = var.cluster_arn
  task_definition                    = aws_ecs_task_definition.service.arn
  desired_count                      = var.desired_count
  launch_type                        = "FARGATE"
  platform_version                   = "1.4.0"
  health_check_grace_period_seconds  = var.load_balancer.health_check_grace_period_seconds
  deployment_minimum_healthy_percent = 100
  deployment_maximum_percent         = 200
  enable_execute_command             = false
  propagate_tags                     = "SERVICE"

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  load_balancer {
    target_group_arn = var.load_balancer.target_group_arn
    container_name   = var.service_name
    container_port   = var.container_port
  }

  network_configuration {
    assign_public_ip = false
    security_groups  = [var.security_group_id]
    subnets          = var.subnet_ids
  }

  # A caller-supplied policy owns this value after service creation. Keeping
  # Terraform out of that feedback loop also avoids erasing a scaled value on
  # unrelated applies; without a policy, the initial count remains stable.
  lifecycle {
    ignore_changes = [desired_count]
  }

  depends_on = [
    aws_iam_role_policy.execution,
    aws_iam_role_policy.execution_secrets,
  ]

  tags = var.tags
}

resource "aws_appautoscaling_target" "service" {
  count = var.scaling == null ? 0 : 1

  max_capacity       = var.scaling == null ? 0 : var.scaling.maximum_tasks
  min_capacity       = var.scaling == null ? 0 : var.scaling.minimum_tasks
  resource_id        = "service/${element(split("/", var.cluster_arn), length(split("/", var.cluster_arn)) - 1)}/${aws_ecs_service.service.name}"
  scalable_dimension = "ecs:service:DesiredCount"
  service_namespace  = "ecs"
}
