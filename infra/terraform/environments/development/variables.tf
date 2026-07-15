variable "aws_region" {
  description = "AWS region for the private-alpha cell."
  type        = string
  default     = "us-east-1"
}

variable "availability_zones" {
  description = "Two availability zones for the private-alpha cell."
  type        = list(string)
  default     = ["us-east-1a", "us-east-1b"]

  validation {
    condition     = length(var.availability_zones) == 2 && length(distinct(var.availability_zones)) == 2 && alltrue([for zone in var.availability_zones : startswith(zone, var.aws_region)])
    error_message = "availability_zones must contain two distinct zones in aws_region."
  }
}

variable "artifact_bucket_name" {
  description = "Globally unique artifact bucket name."
  type        = string
}
