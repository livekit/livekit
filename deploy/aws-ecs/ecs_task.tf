resource "aws_ecs_task_definition" "livekit" {
  family = "service"
  container_definitions = file("livekit.json")
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

  // load balancer for TCP port
  load_balancer {
    target_group_arn = aws_lb_target_group.main.arn
    container_name = "livekit"
    container_port = 7800
  }

  // lifecycle {
  //   ignore_changes = [desired_count]
  // }
}
