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

# target CPU utilization
variable "target_cluster_utilization" {
  type = number
  default = 90
}

# VPC to launch the cluster in
variable "vpc_id" {
  type = string
}

# launch in the following subnet ids, when blank, uses all public subnets in the VPC
variable "subnet_ids" {
  type = list(string)
}

variable "udp_port_start" {
  type = number
  default = 9000
}

variable "udp_port_end" {
  type = number
  default = 9200
}

// variable "ami_id" {
//   type = string
//   // Amazon Linux 2 AMI, for us-east-1
//   default = "ami-08a29dcf20b8fea61"
// }

output "lb_host" {
  value = aws_lb.main.dns_name
}
