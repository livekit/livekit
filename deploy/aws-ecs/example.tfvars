# name of the livekit cluster, resources will be called `livekit-${name}`
name = "demo"

# type of instance to use
instance_type = "t3.small"

# limits to the number of nodes to run
max_nodes = 2

# minimum number of nodes to run
min_nodes = 1

# initially use this number of nodes
nodes = 1

# VPC to create the cluster in
vpc_id = ""

# List of subnet IDs to create the cluster in
subnet_ids = []

# region to use, must match AWS_REGION environment variable
region = "us-east-1"

# additional security groups to attach the cluster to.
# include security group of the redis instance to allow access
security_groups = [
  ""
]

# Livekit configuration

# address and port to redis instance
redis_address = ""

# list of API keys and secrets
api_keys = {
  "key" = "secret"
}

udp_port_start = 9000
udp_port_end = 9100
