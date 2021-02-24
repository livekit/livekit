#
# The following variables need to be set:
# AWS_ACCESS_KEY_ID (or reads from ~/.aws/credentials)
# AWS_SECRET_ACCESS_KEY (or reads from ~/.aws/credentials)
# AWS_REGION
#
provider "aws" {
}

# type of instance to deploy on
# see: https://aws.amazon.com/elasticache/pricing/
variable "instance_type" {
  type = string
  default = "cache.t3.micro"
}

# name of the cluster
variable "cluster_id" {
  type = string
}

# launch in the following subnet ids
variable "subnet_ids" {
  type = list(string)
}

variable "security_groups" {
  type = list(string)
}

output "address" {
  value = "${aws_elasticache_cluster.main.cache_nodes[0].address}:${aws_elasticache_cluster.main.cache_nodes[0].port}"
}