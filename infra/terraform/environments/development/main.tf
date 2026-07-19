locals {
  name_prefix = "sshai-development-${var.aws_region}"
  common_tags = {
    deployment-environment = "development"
    managed-by             = "terraform"
    project                = "sshai"
    region                 = var.aws_region
  }
}

module "regional_cell" {
  source = "../../modules/regional-cell"

  name_prefix          = local.name_prefix
  region               = var.aws_region
  vpc_cidr             = "10.20.0.0/16"
  availability_zones   = var.availability_zones
  public_subnet_cidrs  = ["10.20.0.0/24", "10.20.1.0/24"]
  service_subnet_cidrs = ["10.20.16.0/24", "10.20.17.0/24"]
  runtime_subnet_cidrs = ["10.20.32.0/20", "10.20.48.0/20"]
  tags                 = local.common_tags
}

module "artifact_storage" {
  source = "../../modules/object-storage"

  bucket_name = var.artifact_bucket_name
  tags        = local.common_tags
}

module "postgres" {
  source = "../../modules/rds"

  name_prefix = local.name_prefix
  vpc_id      = module.regional_cell.vpc_id
  subnet_ids  = module.regional_cell.service_subnet_ids
  client_security_group_ids = {
    control_plane = module.regional_cell.ecs_service_security_group_ids.control_plane
    workflows     = module.regional_cell.ecs_service_security_group_ids.workflows
  }
  multi_az = false
  tags     = local.common_tags
}
