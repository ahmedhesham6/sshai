packer {
  required_version = "1.15.4"

  required_plugins {
    amazon = {
      source  = "github.com/hashicorp/amazon"
      version = "= 1.8.1"
    }
  }
}

variable "aws_region" {
  type        = string
  description = "AWS region where the Runtime AMI is built."
  default     = "us-east-1"
}

variable "source_revision" {
  type        = string
  description = "Immutable source revision recorded on the AMI and manifest."

  validation {
    condition     = length(var.source_revision) > 0
    error_message = "The source revision must not be empty."
  }
}

variable "build_vpc_id" {
  type        = string
  description = "Regional cell VPC used for the temporary Packer builder."
}

variable "build_subnet_id" {
  type        = string
  description = "Public regional cell subnet used for the temporary Packer builder."
}

variable "ami_name_prefix" {
  type        = string
  description = "Prefix for versioned Runtime AMI names."
  default     = "sshai-runtime"
}

locals {
  ownership_tags = {
    managed-by      = "packer"
    project         = "sshai"
    source-revision = var.source_revision
  }
}

source "amazon-ebs" "runtime" {
  region                      = var.aws_region
  vpc_id                      = var.build_vpc_id
  subnet_id                   = var.build_subnet_id
  associate_public_ip_address = true
  instance_type               = "t3.small"
  ssh_username                = "ubuntu"

  ami_name        = "${var.ami_name_prefix}-ubuntu-24.04-${formatdate("YYYYMMDDhhmmss", timestamp())}"
  ami_description = "sshai Ubuntu 24.04 x86-64 Runtime image at ${var.source_revision}"
  encrypt_boot    = true
  imds_support    = "v2.0"

  source_ami_filter {
    filters = {
      architecture        = "x86_64"
      name                = "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"
      root-device-type    = "ebs"
      virtualization-type = "hvm"
    }
    most_recent = true
    owners      = ["099720109477"]
  }

  metadata_options {
    http_endpoint               = "enabled"
    http_put_response_hop_limit = 1
    http_tokens                 = "required"
  }

  launch_block_device_mappings {
    device_name           = "/dev/sda1"
    delete_on_termination = true
    encrypted             = true
    volume_size           = 16
    volume_type           = "gp3"
  }

  tags            = merge(local.ownership_tags, { Name = "${var.ami_name_prefix}-ubuntu-24.04" })
  run_tags        = local.ownership_tags
  run_volume_tags = local.ownership_tags
  snapshot_tags   = local.ownership_tags
}

build {
  sources = ["source.amazon-ebs.runtime"]

  provisioner "file" {
    source      = "${path.root}/files/sshai-guest.service"
    destination = "/tmp/sshai-guest.service"
  }

  provisioner "shell" {
    script = "${path.root}/scripts/install-guest-unit.sh"
  }

  post-processor "manifest" {
    output = "${path.root}/manifest.json"
    custom_data = {
      source_revision = var.source_revision
    }
  }
}
