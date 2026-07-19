variable "name_prefix" {
  description = "Prefix used for Runtime image pipeline resources."
  type        = string
}

variable "artifact_bucket_name" {
  description = "Globally unique bucket name for immutable Packer manifests and build logs."
  type        = string
}

variable "source_repository_url" {
  description = "HTTPS URL of the public Git repository CodeBuild checks out for each weekly build."
  type        = string

  validation {
    condition     = startswith(var.source_repository_url, "https://")
    error_message = "source_repository_url must use HTTPS."
  }
}

variable "source_ref" {
  description = "Git branch or ref built by the weekly image pipeline."
  type        = string
  default     = "main"
}

variable "aws_region" {
  description = "Enabled AWS region where the Runtime AMI is built."
  type        = string
  default     = "eu-central-1"
}

variable "build_vpc_id" {
  description = "Regional cell VPC used for the temporary Packer builder."
  type        = string
}

variable "build_subnet_id" {
  description = "Public regional cell subnet used for the temporary Packer builder."
  type        = string
}

variable "schedule_expression" {
  description = "EventBridge schedule for the required weekly image rebuild."
  type        = string
  default     = "rate(7 days)"
}

variable "packer_version" {
  description = "Exact Packer release installed by the hermetic CodeBuild bootstrap."
  type        = string
  default     = "1.15.4"
}

variable "packer_linux_amd64_sha256" {
  description = "SHA-256 for the official linux_amd64 Packer release archive."
  type        = string
  default     = "15f97a6a99645c7d5308c609973b5280837b38e112beac413ccbce80da927cf1"

  validation {
    condition     = can(regex("^[0-9a-f]{64}$", var.packer_linux_amd64_sha256))
    error_message = "packer_linux_amd64_sha256 must be a lowercase SHA-256 digest."
  }
}

variable "tags" {
  description = "Tags applied to image pipeline resources."
  type        = map(string)
  default     = {}
}
