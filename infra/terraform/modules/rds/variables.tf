variable "name_prefix" {
  description = "Prefix used for database resource names."
  type        = string
}

variable "vpc_id" {
  description = "VPC hosting the private database."
  type        = string
}

variable "subnet_ids" {
  description = "Private database subnets in two availability zones."
  type        = list(string)

  validation {
    condition     = length(var.subnet_ids) == 2 && length(distinct(var.subnet_ids)) == 2
    error_message = "subnet_ids must contain exactly two distinct private subnets."
  }
}

variable "client_security_group_ids" {
  description = "Named platform task security groups permitted to connect to PostgreSQL."
  type        = map(string)

  validation {
    condition     = length(var.client_security_group_ids) > 0
    error_message = "At least one platform client security group is required."
  }
}

variable "instance_class" {
  description = "RDS instance class for the private-alpha database."
  type        = string
  default     = "db.t4g.micro"
}

variable "tags" {
  description = "Tags applied to database resources."
  type        = map(string)
}
