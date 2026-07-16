variable "bucket_name" {
  description = "Globally unique name for the owner-scoped Capsule store bucket."
  type        = string
}

variable "tags" {
  description = "Tags applied to Capsule store resources."
  type        = map(string)
  default     = {}
}
