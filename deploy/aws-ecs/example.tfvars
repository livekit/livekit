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

# UDP port range for WebRTC, uncomment to override
// udp_port_start = 9000
// udp_port_end = 11000

# Use embedded TURN server, defaults true
// turn_enabled = true
// turn_tcp_port = 3478
// turn_udp_port = 3479

# UDP port range for embedded TURN server
// turn_port_start = 11001
// turn_port_end = 13000

