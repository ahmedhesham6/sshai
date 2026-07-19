mock_provider "aws" {}

variables {
  name = "sshai-development-eu-central-1"
  tags = {
    managed-by = "terraform"
    project    = "sshai"
  }
}

run "creates_an_observable_fargate_cluster" {
  command = apply

  assert {
    condition     = one(aws_ecs_cluster.platform.setting).name == "containerInsights" && one(aws_ecs_cluster.platform.setting).value == "enabled"
    error_message = "The cluster must publish Container Insights telemetry."
  }

  assert {
    condition     = aws_ecs_cluster_capacity_providers.platform.capacity_providers == toset(["FARGATE"])
    error_message = "The platform cluster must use Fargate capacity."
  }
}
