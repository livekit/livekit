resource "aws_ecs_task_definition" "livekit" {
  family = "service"
  container_definitions = jsonencode(local.task_config)
  network_mode = "host"
  execution_role_arn = aws_iam_role.ecs_role.arn
}

resource "aws_ecs_service" "livekit" {
  name = "livekit-${var.name}"
  cluster = module.app_ecs_cluster.ecs_cluster_arn
  task_definition = aws_ecs_task_definition.livekit.arn
  desired_count = var.nodes
  force_new_deployment = true
  launch_type = "EC2"

  placement_constraints {
    // one instance per node
    type = "distinctInstance"
  }

  ordered_placement_strategy {
    type = "spread"
    field = "instanceId"
  }

  // load balancer for HTTP port
  load_balancer {
    target_group_arn = aws_lb_target_group.http.arn
    container_name = "livekit"
    container_port = var.http_port
  }

  depends_on = [
    aws_lb_listener.http
  ]

  // lifecycle {
  //   ignore_changes = [desired_count]
  // }
}

resource "aws_cloudwatch_log_group" "livekit" {
  name = "livekit"

  retention_in_days = 7
}
