mock_provider "aws" {}

variables {
  name_prefix = "sshai-development-us-east-1"
  vpc_id      = "vpc-0123456789abcdef0"
  subnet_ids  = ["subnet-00000000000000001", "subnet-00000000000000002"]
  client_security_group_ids = {
    control_plane = "sg-00000000000000001"
    workflows     = "sg-00000000000000002"
  }
  tags = {
    managed-by = "terraform"
    project    = "sshai"
  }
}

run "creates_private_recoverable_postgres" {
  command = apply

  assert {
    condition     = aws_db_instance.postgres.engine == "postgres" && aws_db_instance.postgres.engine_version == "18.4"
    error_message = "The platform database must pin the supported PostgreSQL engine."
  }

  assert {
    condition     = aws_db_instance.postgres.storage_encrypted && !aws_db_instance.postgres.publicly_accessible
    error_message = "The database must be encrypted and private."
  }

  assert {
    condition     = aws_db_instance.postgres.manage_master_user_password
    error_message = "RDS must manage the master password outside Terraform state."
  }

  assert {
    condition     = aws_db_instance.postgres.backup_retention_period >= 7 && aws_db_instance.postgres.deletion_protection && !aws_db_instance.postgres.skip_final_snapshot
    error_message = "The database must retain recoverable backups and resist accidental deletion."
  }

  assert {
    condition     = length(aws_db_subnet_group.postgres.subnet_ids) == 2 && !aws_db_instance.postgres.multi_az
    error_message = "Private alpha uses a Single-AZ instance with a two-zone subnet baseline."
  }

  assert {
    condition     = length(aws_vpc_security_group_ingress_rule.postgres) == 2 && alltrue([for rule in aws_vpc_security_group_ingress_rule.postgres : rule.from_port == 5432 && rule.to_port == 5432 && contains(values(var.client_security_group_ids), rule.referenced_security_group_id)])
    error_message = "PostgreSQL ingress must be limited to declared platform task security groups."
  }

  assert {
    condition     = length(aws_security_group.postgres.ingress) == 0
    error_message = "The database security group must not contain inline or CIDR ingress."
  }
}

run "rejects_a_single_database_subnet" {
  command = plan

  variables {
    subnet_ids = ["subnet-00000000000000001"]
  }

  expect_failures = [var.subnet_ids]
}
