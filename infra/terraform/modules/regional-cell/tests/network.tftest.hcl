mock_provider "aws" {}

variables {
  name_prefix          = "sshai-development-us-east-1"
  region               = "us-east-1"
  vpc_cidr             = "10.20.0.0/16"
  availability_zones   = ["us-east-1a", "us-east-1b"]
  public_subnet_cidrs  = ["10.20.0.0/24", "10.20.1.0/24"]
  service_subnet_cidrs = ["10.20.16.0/24", "10.20.17.0/24"]
  runtime_subnet_cidrs = ["10.20.32.0/20", "10.20.48.0/20"]
  tags = {
    managed-by = "terraform"
    project    = "sshai"
  }
}

run "creates_private_regional_cell" {
  command = apply

  assert {
    condition     = length(aws_subnet.public) == 2 && alltrue([for subnet in aws_subnet.public : subnet.map_public_ip_on_launch])
    error_message = "The cell must expose two public subnets for load balancers and NAT."
  }

  assert {
    condition     = length(aws_subnet.runtime) == 2 && alltrue([for subnet in aws_subnet.runtime : !subnet.map_public_ip_on_launch])
    error_message = "Runtime subnets must remain private."
  }

  assert {
    condition     = length(aws_subnet.service) == 2 && alltrue([for subnet in aws_subnet.service : !subnet.map_public_ip_on_launch])
    error_message = "Platform service subnets must remain private."
  }

  assert {
    condition     = aws_nat_gateway.egress.subnet_id == aws_subnet.public["0"].id
    error_message = "Private alpha must place its single managed NAT egress path in the first public subnet."
  }

  assert {
    condition     = aws_route.private_egress.nat_gateway_id == aws_nat_gateway.egress.id
    error_message = "Private traffic must use the managed NAT gateway."
  }

  assert {
    condition     = aws_vpc_security_group_ingress_rule.runtime_ssh.from_port == 22 && aws_vpc_security_group_ingress_rule.runtime_ssh.to_port == 22 && aws_vpc_security_group_ingress_rule.runtime_ssh.referenced_security_group_id == aws_security_group.ssh_proxy.id
    error_message = "Runtime SSH ingress must come only from the regional proxy security group."
  }

  assert {
    condition     = length(aws_security_group.runtime.ingress) == 0
    error_message = "Runtime security groups must not contain inline or public ingress."
  }

  assert {
    condition     = aws_vpc_endpoint.s3.vpc_endpoint_type == "Gateway"
    error_message = "The regional cell must provide an S3 gateway endpoint."
  }

  assert {
    condition     = length(distinct(values(output.ecs_service_security_group_ids))) == 3
    error_message = "Control-plane, workflow, and SSH proxy tasks need separate security groups."
  }
}

run "rejects_a_single_availability_zone" {
  command = plan

  variables {
    availability_zones = ["us-east-1a"]
  }

  expect_failures = [var.availability_zones]
}
