locals {
  subnet_indexes = toset(["0", "1"])
}

resource "aws_vpc" "cell" {
  cidr_block           = var.vpc_cidr
  enable_dns_hostnames = true
  enable_dns_support   = true

  tags = merge(var.tags, { Name = "${var.name_prefix}-vpc" })
}

resource "aws_internet_gateway" "cell" {
  vpc_id = aws_vpc.cell.id

  tags = merge(var.tags, { Name = "${var.name_prefix}-igw" })
}

resource "aws_subnet" "public" {
  for_each = local.subnet_indexes

  vpc_id                  = aws_vpc.cell.id
  availability_zone       = var.availability_zones[tonumber(each.key)]
  cidr_block              = var.public_subnet_cidrs[tonumber(each.key)]
  map_public_ip_on_launch = true

  tags = merge(var.tags, { Name = "${var.name_prefix}-public-${each.key}" })
}

resource "aws_subnet" "service" {
  for_each = local.subnet_indexes

  vpc_id                  = aws_vpc.cell.id
  availability_zone       = var.availability_zones[tonumber(each.key)]
  cidr_block              = var.service_subnet_cidrs[tonumber(each.key)]
  map_public_ip_on_launch = false

  tags = merge(var.tags, { Name = "${var.name_prefix}-service-${each.key}" })
}

resource "aws_subnet" "runtime" {
  for_each = local.subnet_indexes

  vpc_id                  = aws_vpc.cell.id
  availability_zone       = var.availability_zones[tonumber(each.key)]
  cidr_block              = var.runtime_subnet_cidrs[tonumber(each.key)]
  map_public_ip_on_launch = false

  tags = merge(var.tags, { Name = "${var.name_prefix}-runtime-${each.key}" })
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.cell.id
  tags   = merge(var.tags, { Name = "${var.name_prefix}-public" })
}

resource "aws_route" "public_internet" {
  route_table_id         = aws_route_table.public.id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.cell.id
}

resource "aws_route_table_association" "public" {
  for_each = aws_subnet.public

  route_table_id = aws_route_table.public.id
  subnet_id      = each.value.id
}

resource "aws_eip" "nat" {
  domain = "vpc"
  tags   = merge(var.tags, { Name = "${var.name_prefix}-nat" })
}

resource "aws_nat_gateway" "egress" {
  allocation_id = aws_eip.nat.id
  subnet_id     = aws_subnet.public["0"].id

  tags       = merge(var.tags, { Name = "${var.name_prefix}-egress" })
  depends_on = [aws_internet_gateway.cell]
}

resource "aws_route_table" "private" {
  vpc_id = aws_vpc.cell.id
  tags   = merge(var.tags, { Name = "${var.name_prefix}-private" })
}

resource "aws_route" "private_egress" {
  route_table_id         = aws_route_table.private.id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = aws_nat_gateway.egress.id
}

resource "aws_route_table_association" "service" {
  for_each = aws_subnet.service

  route_table_id = aws_route_table.private.id
  subnet_id      = each.value.id
}

resource "aws_route_table_association" "runtime" {
  for_each = aws_subnet.runtime

  route_table_id = aws_route_table.private.id
  subnet_id      = each.value.id
}

resource "aws_vpc_endpoint" "s3" {
  vpc_id            = aws_vpc.cell.id
  service_name      = "com.amazonaws.${var.region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = [aws_route_table.private.id]

  tags = merge(var.tags, { Name = "${var.name_prefix}-s3" })
}

resource "aws_security_group" "load_balancer" {
  name        = "${var.name_prefix}-load-balancer"
  description = "Public TLS entrypoint for regional services"
  vpc_id      = aws_vpc.cell.id

  tags = merge(var.tags, { Name = "${var.name_prefix}-load-balancer" })
}

resource "aws_vpc_security_group_ingress_rule" "load_balancer_https" {
  security_group_id = aws_security_group.load_balancer.id
  description       = "Public HTTPS"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443

  tags = var.tags
}

resource "aws_security_group" "control_plane" {
  name        = "${var.name_prefix}-control-plane"
  description = "Control-plane ECS tasks"
  vpc_id      = aws_vpc.cell.id

  tags = merge(var.tags, { Name = "${var.name_prefix}-control-plane" })
}

resource "aws_security_group" "workflows" {
  name        = "${var.name_prefix}-workflows"
  description = "Workflow ECS tasks"
  vpc_id      = aws_vpc.cell.id

  tags = merge(var.tags, { Name = "${var.name_prefix}-workflows" })
}

resource "aws_security_group" "ssh_proxy" {
  name        = "${var.name_prefix}-ssh-proxy"
  description = "Regional SSH proxy ECS tasks"
  vpc_id      = aws_vpc.cell.id

  tags = merge(var.tags, { Name = "${var.name_prefix}-ssh-proxy" })
}

locals {
  ecs_service_security_groups = {
    control_plane = aws_security_group.control_plane.id
    workflows     = aws_security_group.workflows.id
    ssh_proxy     = aws_security_group.ssh_proxy.id
  }
}

resource "aws_vpc_security_group_ingress_rule" "ecs_from_load_balancer" {
  for_each = local.ecs_service_security_groups

  security_group_id            = each.value
  description                  = "Service traffic from the managed load balancer"
  referenced_security_group_id = aws_security_group.load_balancer.id
  ip_protocol                  = "tcp"
  from_port                    = var.service_port
  to_port                      = var.service_port

  tags = var.tags
}

resource "aws_vpc_security_group_egress_rule" "load_balancer_to_ecs" {
  for_each = local.ecs_service_security_groups

  security_group_id            = aws_security_group.load_balancer.id
  description                  = "Traffic to ${replace(each.key, "_", " ")} tasks"
  referenced_security_group_id = each.value
  ip_protocol                  = "tcp"
  from_port                    = var.service_port
  to_port                      = var.service_port

  tags = var.tags
}

resource "aws_vpc_security_group_egress_rule" "ecs_outbound" {
  for_each = local.ecs_service_security_groups

  security_group_id = each.value
  description       = "Outbound service traffic through managed egress"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"

  tags = var.tags
}

resource "aws_security_group" "runtime" {
  name        = "${var.name_prefix}-runtime"
  description = "Private Runtime instances"
  vpc_id      = aws_vpc.cell.id

  tags = merge(var.tags, { Name = "${var.name_prefix}-runtime" })
}

resource "aws_vpc_security_group_ingress_rule" "runtime_ssh" {
  security_group_id            = aws_security_group.runtime.id
  description                  = "SSH from the regional proxy only"
  referenced_security_group_id = aws_security_group.ssh_proxy.id
  ip_protocol                  = "tcp"
  from_port                    = 22
  to_port                      = 22

  tags = var.tags
}

resource "aws_vpc_security_group_egress_rule" "runtime_outbound" {
  security_group_id = aws_security_group.runtime.id
  description       = "Runtime internet and AWS service egress"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"

  tags = var.tags
}
