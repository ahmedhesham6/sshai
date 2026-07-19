mock_provider "aws" {}

variables {
  aws_region           = "eu-central-1"
  availability_zones   = ["eu-central-1a", "eu-central-1b"]
  artifact_bucket_name = "sshai-development-eu-central-1-artifacts"
}

run "assembles_one_private_alpha_cell" {
  command = apply

  assert {
    condition     = length(output.public_subnet_ids) == 2 && length(output.runtime_subnet_ids) == 2
    error_message = "The root must expose the public and private Runtime placement interfaces."
  }

  assert {
    condition     = length(distinct(values(output.ecs_service_security_group_ids))) == 3
    error_message = "The root must preserve separate ECS service security groups."
  }

  assert {
    condition     = output.artifact_bucket_id != ""
    error_message = "The root must expose its artifact bucket ID."
  }

  assert {
    condition     = output.database_port == 5432
    error_message = "The root must expose the private PostgreSQL port."
  }
}

run "rejects_zones_outside_the_cell_region" {
  command = plan

  variables {
    availability_zones = ["us-west-2a", "us-west-2b"]
  }

  expect_failures = [var.availability_zones]
}
