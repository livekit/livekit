data "aws_vpc" "main" {
  id = var.vpc_id
}

resource "aws_security_group" "main" {
  name        = "livekit-${var.name}"
  description = "Allow LiveKit inbound TCP and UDP traffic"
  vpc_id      = data.aws_vpc.main.id

  ingress {
    description = "UDP port for ICE"
    from_port   = var.udp_port_start
    to_port     = var.udp_port_end
    protocol    = "udp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  ingress {
    description = "internal traffic"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = [data.aws_vpc.main.cidr_block]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name = "livekit"
  }
}

resource "aws_security_group" "lb" {
  name        = "livekit-${var.name}-lb"
  description = "Load balancer traffic"
  vpc_id      = data.aws_vpc.main.id

  ingress {
    description = "HTTP"
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  ingress {
    description = "HTTPS"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name = "livekit"
  }
}
