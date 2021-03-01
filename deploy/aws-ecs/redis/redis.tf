resource "aws_elasticache_cluster" "main" {
  cluster_id           = var.cluster_id
  engine               = "redis"
  node_type            = var.instance_type
  num_cache_nodes      = 1
  parameter_group_name = "default.redis6.x"
  # strangely terraform requires you to change to a specific version when
  # modifying the resource.
  engine_version       = "6.x"
  subnet_group_name    = aws_elasticache_subnet_group.main.name
  security_group_ids   = var.security_groups
  port                 = 6379
}

resource "aws_elasticache_subnet_group" "main" {
  name       = var.cluster_id
  subnet_ids = var.subnet_ids
}
