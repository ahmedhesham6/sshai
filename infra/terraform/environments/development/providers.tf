provider "aws" {
  region = var.aws_region

  default_tags {
    tags = {
      deployment-environment = "development"
      managed-by             = "terraform"
      project                = "sshai"
      region                 = var.aws_region
    }
  }
}
