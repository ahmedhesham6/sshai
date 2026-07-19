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
  default     = "eu-central-1"
}

variable "source_revision" {
  type        = string
  description = "Immutable source revision recorded on the AMI and manifest."

  validation {
    condition     = can(regex("^[0-9a-f]{40}$", var.source_revision))
    error_message = "The source revision must be a lowercase 40-hex Git commit ID."
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

variable "pipeline_id" {
  type        = string
  description = "Ownership tag identifying the image pipeline allowed to mutate build resources."

  validation {
    condition     = length(trimspace(var.pipeline_id)) > 0
    error_message = "The pipeline ID must not be empty."
  }
}

variable "guest_binary_source" {
  type        = string
  description = "Local path to the prebuilt guest supervisor binary uploaded into the image."

  validation {
    condition     = length(trimspace(var.guest_binary_source)) > 0
    error_message = "The guest binary source must not be empty."
  }
}

variable "validate_only" {
  type        = bool
  description = "Allows static template validation without a published guest artifact; image builds reject this mode."
  default     = false
}

variable "ami_kms_key_id" {
  type        = string
  description = "Optional KMS key ID or ARN used for the encrypted final AMI copy."
  default     = ""
}

variable "claude_code_version" {
  type        = string
  description = "Exact Claude Code version installed in the platform-owned system image."
  default     = "2.1.215"
}

variable "codex_version" {
  type        = string
  description = "Exact Codex version installed in the platform-owned system image."
  default     = "0.144.6"
}

variable "opencode_version" {
  type        = string
  description = "Exact OpenCode version installed in the platform-owned system image."
  default     = "1.18.3"
}

variable "ami_name_prefix" {
  type        = string
  description = "Prefix for versioned Runtime AMI names."
  default     = "sshai-runtime"
}

locals {
  ownership_tags = {
    managed-by      = "packer"
    image-pipeline  = var.pipeline_id
    project         = "sshai"
    source-revision = var.source_revision
  }
}

source "amazon-ebs" "runtime" {
  region                                    = var.aws_region
  vpc_id                                    = var.build_vpc_id
  subnet_id                                 = var.build_subnet_id
  associate_public_ip_address               = true
  temporary_security_group_source_public_ip = true
  instance_type                             = "t3.small"
  ssh_username                              = "ubuntu"

  ami_name        = "${var.ami_name_prefix}-ubuntu-24.04-${formatdate("YYYYMMDDhhmmss", timestamp())}"
  ami_description = "sshai Ubuntu 24.04 x86-64 Runtime image at ${var.source_revision}"
  encrypt_boot    = true
  kms_key_id      = var.ami_kms_key_id != "" ? var.ami_kms_key_id : null
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
    volume_size           = 30
    volume_type           = "gp3"
  }

  # run_tags are applied to the intermediate AMI/snapshot at creation and the
  # amazon plugin copies the AMI tags to the encrypted result. Do not set
  # tags/snapshot_tags here: their final standalone CreateTags call cannot be
  # ownership-scoped safely while the copied backing snapshot is still untagged.
  run_tags        = local.ownership_tags
  run_volume_tags = local.ownership_tags
}

build {
  sources = ["source.amazon-ebs.runtime"]

  provisioner "file" {
    source      = "${path.root}/files/sshai-guest.service"
    destination = "/tmp/sshai-guest.service"
  }

  # Spec 13 requires a checksum gate for the guest supervisor. The source is
  # explicit now, but checksum verification remains deferred until a published
  # guest artifact and its independently hosted digest exist.
  provisioner "file" {
    source      = var.guest_binary_source
    destination = "/tmp/sshai-guest"
  }

  provisioner "shell" {
    environment_vars = ["VALIDATE_ONLY=${var.validate_only}"]
    script           = "${path.root}/scripts/install-guest-unit.sh"
  }

  provisioner "shell" {
    environment_vars = [
      "CLAUDE_CODE_VERSION=${var.claude_code_version}",
      "CODEX_VERSION=${var.codex_version}",
      "OPENCODE_VERSION=${var.opencode_version}",
    ]
    script = "${path.root}/scripts/install-runtime-tooling.sh"
  }

  provisioner "shell" {
    script = "${path.root}/scripts/configure-home-first-tooling.sh"
  }

  provisioner "shell" {
    script = "${path.root}/scripts/configure-openssh.sh"
  }

  provisioner "shell" {
    environment_vars = [
      "CLAUDE_CODE_VERSION=${var.claude_code_version}",
      "CODEX_VERSION=${var.codex_version}",
      "OPENCODE_VERSION=${var.opencode_version}",
      "VALIDATE_ONLY=${var.validate_only}",
    ]
    script = "${path.root}/scripts/validate-runtime.sh"
  }

  # The reconnect proves the image remains reachable after a reboot. Deeper
  # guest readiness and attached-data-volume checks require the credentialed
  # EC2 harness and remain deferred build gates.
  provisioner "shell" {
    inline            = ["sudo shutdown -r now"]
    expect_disconnect = true
    pause_after       = "30s"
  }

  provisioner "shell" {
    environment_vars = [
      "CLAUDE_CODE_VERSION=${var.claude_code_version}",
      "CODEX_VERSION=${var.codex_version}",
      "OPENCODE_VERSION=${var.opencode_version}",
      "SOURCE_REVISION=${var.source_revision}",
      "VALIDATE_ONLY=${var.validate_only}",
    ]
    script = "${path.root}/scripts/finalize-build-gates.sh"
  }

  post-processor "manifest" {
    output = "${path.root}/manifest.json"
    custom_data = {
      source_revision     = var.source_revision
      claude_code_version = var.claude_code_version
      codex_version       = var.codex_version
      opencode_version    = var.opencode_version
    }
  }
}
