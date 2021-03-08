locals {
  livekit_config = {
    port = var.http_port
    rtc = {
      port_range_start = var.udp_port_start
      port_range_end = var.udp_port_end
    }
    turn = {
      enabled = var.turn_enabled
      tcp_port = var.turn_tcp_port
      udp_port = var.turn_udp_port
      port_range_start = var.turn_port_start
      port_range_end = var.turn_port_end
    }
    development = true
    keys = var.api_keys
    redis = {
      address = var.redis_address
    }
  }

  // mapping contains only the main listening ports
  // other UDP ports don't have to be mapped, due to
  port_mapping = [
    {
      containerPort = var.http_port
      protocol = "tcp"
    },
    {
      containerPort = var.turn_tcp_port
      protocol = "tcp"
    },
    {
      containerPort = var.turn_udp_port
      protocol = "udp"
    },
  ]

  task_config = [{
    name = "livekit"
    image = "livekit/livekit-server:${var.livekit_version}"
    cpu = 1024
    memory = 1024
    essential = true
    environment = [
      {
        name = "LIVEKIT_CONFIG"
        value = yamlencode(local.livekit_config)
      }
    ]
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        "awslogs-region" = var.region
        "awslogs-group" = "livekit"
        "awslogs-stream-prefix" = var.name
      }
    },
    portMappings = local.port_mapping
  }]
}