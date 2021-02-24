data "aws_ami" "ecs_ami" {
  most_recent = true
  owners      = ["amazon"]

  filter {
    name   = "name"
    values = ["amzn-ami-*-amazon-ecs-optimized"]
  }
}

module "app_ecs_cluster" {
  source = "trussworks/ecs-cluster/aws"

  name        = "livekit"
  environment = var.name

  image_id      = data.aws_ami.ecs_ami.image_id
  instance_type = var.instance_type

  vpc_id           = var.vpc_id
  subnet_ids       = var.subnet_ids
  security_group_ids = concat(var.security_groups, [aws_security_group.main.id])
  desired_capacity = var.nodes
  max_size         = var.max_nodes
  min_size         = var.min_nodes
}
