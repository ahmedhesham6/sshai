variable "name" {
  description = "Name of the shared ECS Fargate cluster."
  type        = string
}

variable "tags" {
  description = "Tags applied to ECS cluster resources."
  type        = map(string)
  default     = {}
}
