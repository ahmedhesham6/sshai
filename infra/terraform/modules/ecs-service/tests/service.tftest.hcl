mock_provider "aws" {
  mock_resource "aws_iam_role" {
    defaults = {
      arn = "arn:aws:iam::123456789012:role/mock-ecs-service-role"
    }
  }

  mock_resource "aws_ecs_task_definition" {
    defaults = {
      arn = "arn:aws:ecs:eu-central-1:123456789012:task-definition/mock-service:1"
    }
  }

  mock_data "aws_region" {
    defaults = {
      region = "eu-central-1"
    }
  }
}

variables {
  name_prefix              = "sshai-development-eu-central-1"
  service_name             = "control-plane"
  cluster_arn              = "arn:aws:ecs:eu-central-1:123456789012:cluster/sshai-development-eu-central-1"
  container_image          = "123456789012.dkr.ecr.eu-central-1.amazonaws.com/control-plane@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  container_repository_arn = "arn:aws:ecr:eu-central-1:123456789012:repository/control-plane"
  container_port           = 8080
  task_cpu                 = 512
  task_memory              = 1024
  desired_count            = 1
  subnet_ids               = ["subnet-00000000000000001", "subnet-00000000000000002"]
  security_group_id        = "sg-00000000000000001"
  load_balancer = {
    target_group_arn                  = "arn:aws:elasticloadbalancing:eu-central-1:123456789012:targetgroup/control-plane/0123456789abcdef"
    health_check_grace_period_seconds = 30
    websocket_idle_timeout_seconds    = 3600
    client_keep_alive_seconds         = 3600
    maximum_connection_age_seconds    = 86400
  }
  task_role_policy_json = jsonencode({
    Version   = "2012-10-17"
    Statement = []
  })
  secrets = {
    DATABASE_URL = "arn:aws:secretsmanager:eu-central-1:123456789012:secret:development/database"
  }
  tags = {
    managed-by = "terraform"
    project    = "sshai"
  }
}

run "creates_an_isolated_private_fargate_service" {
  command = apply

  assert {
    condition     = aws_ecs_task_definition.service.requires_compatibilities == toset(["FARGATE"]) && aws_ecs_task_definition.service.network_mode == "awsvpc"
    error_message = "Platform services must run as Fargate tasks with awsvpc networking."
  }

  assert {
    condition     = !aws_ecs_service.service.network_configuration[0].assign_public_ip && aws_ecs_service.service.network_configuration[0].security_groups == toset([var.security_group_id])
    error_message = "Service tasks must remain private and use only their distinct security group."
  }

  assert {
    condition     = one(aws_ecs_service.service.load_balancer).target_group_arn == var.load_balancer.target_group_arn
    error_message = "The service must use the explicitly supplied unresolved load-balancer decision."
  }

  assert {
    condition     = aws_iam_role.task.name != aws_iam_role.execution.name && aws_ecs_task_definition.service.task_role_arn == aws_iam_role.task.arn
    error_message = "Each service needs a distinct application role and deployment execution role."
  }

  assert {
    condition = (
      one([for statement in jsondecode(aws_iam_role_policy.execution.policy).Statement : statement if statement.Sid == "AuthenticateToEcr"]).Resource == "*" &&
      one([for statement in jsondecode(aws_iam_role_policy.execution.policy).Statement : statement if statement.Sid == "PullServiceImage"]).Resource == var.container_repository_arn &&
      one([for statement in jsondecode(aws_iam_role_policy.execution.policy).Statement : statement if statement.Sid == "WriteServiceLogs"]).Resource == "${aws_cloudwatch_log_group.service.arn}:*"
    )
    error_message = "The execution role must scope image pulls and log writes while leaving only ECR authentication wildcarded."
  }

  assert {
    condition     = length(aws_iam_role_policy.execution_secrets) == 1 && strcontains(one(aws_iam_role_policy.execution_secrets).policy, var.secrets.DATABASE_URL)
    error_message = "The execution role must be able to retrieve only the declared injected secrets."
  }

  assert {
    condition     = length(aws_appautoscaling_target.service) == 0 && output.autoscaling_target == null
    error_message = "The module must create no scaling resources or choose a scaling signal when scaling is omitted."
  }

  assert {
    condition     = one(aws_ecs_service.service.deployment_circuit_breaker).enable && one(aws_ecs_service.service.deployment_circuit_breaker).rollback
    error_message = "Failed deployments must roll back automatically."
  }
}

run "rejects_a_mutable_container_tag" {
  command = plan

  variables {
    container_image = "example.invalid/control-plane:latest"
  }

  expect_failures = [var.container_image]
}

run "rejects_an_image_outside_the_scoped_repository" {
  command = plan

  variables {
    container_image = "123456789012.dkr.ecr.eu-central-1.amazonaws.com/workflows@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  }

  expect_failures = [aws_ecs_task_definition.service]
}
