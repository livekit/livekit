#
# The following variables need to be set:
# AWS_ACCESS_KEY_ID (or reads from ~/.aws/credentials)
# AWS_SECRET_ACCESS_KEY (or reads from ~/.aws/credentials)
# AWS_REGION
#
provider "aws" {
}

# name of the cluster
variable "name" {
  type = string
}

# type of instance to deploy on
variable "instance_type" {
  type = string
  default = "t3.small"
}

variable "nodes" {
  type = number
  default = 1
}

variable "min_nodes" {
  type = number
  default = 1
}

variable "max_nodes" {
  type = number
}

variable "region" {
  type = string
}

# target CPU utilization
variable "target_cluster_utilization" {
  type = number
  default = 90
}

# VPC to launch the cluster in
variable "vpc_id" {
  type = string
}

# launch in the following subnet ids
variable "subnet_ids" {
  type = list(string)
}

# additional security groups to associate with
# i.e. security group of the Redis instance
variable "security_groups" {
  type = list(string)
  default = []
}

# livekit config
variable "livekit_version" {
  type = string
  default = "latest"
}

variable "http_port" {
  type = number
  default = 7880
}

variable "udp_port_start" {
  type = number
  default = 9000
}

variable "udp_port_end" {
  type = number
  default = 9100
}

variable "api_keys" {
  type = map(string)
}

variable "redis_address" {
  type = string
  default = ""
}

output "livekit_lb" {
  value = aws_lb.main.dns_name
}
