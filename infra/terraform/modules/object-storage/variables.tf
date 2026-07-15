variable "bucket_name" {
  description = "Globally unique name for the artifact bucket."
  type        = string
}

variable "tags" {
  description = "Tags applied to artifact storage resources."
  type        = map(string)
}
