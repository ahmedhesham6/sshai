resource "aws_db_subnet_group" "postgres" {
  name       = "${var.name_prefix}-postgres"
  subnet_ids = var.subnet_ids

  tags = merge(var.tags, { Name = "${var.name_prefix}-postgres" })
}

resource "aws_security_group" "postgres" {
  name        = "${var.name_prefix}-postgres"
  description = "Private PostgreSQL access from platform tasks"
  vpc_id      = var.vpc_id

  tags = merge(var.tags, { Name = "${var.name_prefix}-postgres" })
}

resource "aws_vpc_security_group_ingress_rule" "postgres" {
  for_each = var.client_security_group_ids

  security_group_id            = aws_security_group.postgres.id
  description                  = "PostgreSQL from an authorized platform task"
  referenced_security_group_id = each.value
  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432

  tags = var.tags
}

resource "aws_db_instance" "postgres" {
  identifier = "${var.name_prefix}-postgres"

  engine         = "postgres"
  engine_version = "18.4"
  instance_class = var.instance_class

  db_name                     = "sshai"
  username                    = "sshai_admin"
  manage_master_user_password = true
  port                        = 5432

  allocated_storage     = 20
  max_allocated_storage = 100
  storage_type          = "gp3"
  storage_encrypted     = true

  db_subnet_group_name   = aws_db_subnet_group.postgres.name
  vpc_security_group_ids = [aws_security_group.postgres.id]
  publicly_accessible    = false
  multi_az               = false

  backup_retention_period    = 7
  backup_window              = "03:00-04:00"
  maintenance_window         = "sun:04:00-sun:05:00"
  auto_minor_version_upgrade = true
  copy_tags_to_snapshot      = true
  delete_automated_backups   = false
  deletion_protection        = true
  skip_final_snapshot        = false
  final_snapshot_identifier  = "${var.name_prefix}-postgres-final"

  enabled_cloudwatch_logs_exports = ["postgresql", "upgrade"]

  tags = merge(var.tags, { Name = "${var.name_prefix}-postgres" })
}
