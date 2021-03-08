

// configure target group
resource "aws_lb_target_group" "http" {
  name = "livekit-${var.name}-http"
  port = 80
  protocol = "HTTP"
  vpc_id = data.aws_vpc.main.id
}

resource "aws_lb" "main" {
  name = "livekit-${var.name}-http"
  internal           = false
  load_balancer_type = "application"
  security_groups    = [aws_security_group.lb.id]
  subnets = var.subnet_ids
}

resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.main.arn
  port              = "80"
  protocol          = "HTTP"
  // ssl_policy        = "ELBSecurityPolicy-2016-08"

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.http.arn
  }
}
