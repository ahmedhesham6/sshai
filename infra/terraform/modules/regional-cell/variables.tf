variable "name_prefix" {
  description = "Prefix used for regional cell resource names."
  type        = string
}

variable "vpc_cidr" {
  description = "IPv4 CIDR for the regional cell VPC."
  type        = string
}

variable "region" {
  description = "AWS region containing the cell."
  type        = string
}

variable "availability_zones" {
  description = "Two availability zones used by the private-alpha cell."
  type        = list(string)

  validation {
    condition     = length(var.availability_zones) == 2 && length(distinct(var.availability_zones)) == 2
    error_message = "availability_zones must contain exactly two distinct zones."
  }
}

variable "public_subnet_cidrs" {
  description = "CIDRs for public load-balancer and NAT subnets."
  type        = list(string)

  validation {
    condition     = length(var.public_subnet_cidrs) == 2
    error_message = "public_subnet_cidrs must contain exactly two CIDRs."
  }
}

variable "service_subnet_cidrs" {
  description = "CIDRs for private platform service subnets."
  type        = list(string)

  validation {
    condition     = length(var.service_subnet_cidrs) == 2
    error_message = "service_subnet_cidrs must contain exactly two CIDRs."
  }
}

variable "runtime_subnet_cidrs" {
  description = "CIDRs for private Runtime subnets."
  type        = list(string)

  validation {
    condition     = length(var.runtime_subnet_cidrs) == 2
    error_message = "runtime_subnet_cidrs must contain exactly two CIDRs."
  }
}

variable "service_port" {
  description = "Container port reached from the public load balancer."
  type        = number
  default     = 8080

  validation {
    condition     = var.service_port >= 1 && var.service_port <= 65535
    error_message = "service_port must be a valid TCP port."
  }
}

variable "tags" {
  description = "Tags applied to every regional cell resource."
  type        = map(string)
}
