packer {
  required_plugins {
    amazon = {
      version = ">= 0.0.2"
      source  = "github.com/hashicorp/amazon"
    }
  }
}

locals {
  livekit_server_version = "v0.13"
}

source "amazon-ebs" "amazon-linux-2" {
  ami_name      = "livekit-server amzn2 {{timestamp}}"
  instance_type = "t2.micro"
  region        = "us-west-2"
  source_ami_filter {
    filters = {
      name                = "amzn2-ami-hvm-2.0.*-x86_64-gp2"
      root-device-type    = "ebs"
      virtualization-type = "hvm"
    }
    most_recent = true
    owners      = ["137112412989"] // amazon
  }
  ssh_username = "ec2-user"
}

build {
  name = "livekit"
  sources = [
    "source.amazon-ebs.amazon-linux-2"
  ]

  # # Config should be provided during cloud-init
  # provisioner "file" {
  #   source      = "config.yaml"
  #   destination = "/tmp/config.yaml"
  # }

  provisioner "file" {
    source      = "docker.livekit-server@.service"
    destination = "/tmp/docker.livekit-server@.service"
  }

  provisioner "shell" {
    environment_vars = []
    inline = [
      # docker
      "sudo yum update -y",
      "sudo yum install -y docker",
      "sudo systemctl enable docker",
      "sudo systemctl start docker",

      # livekit
      "sudo mv /tmp/docker.livekit-server@.service /etc/systemd/system/docker.livekit-server@.service",
      "sudo chown root:root /etc/systemd/system/docker.livekit-server@.service",
      "sudo mkdir /opt/livekit-server",
      # "sudo mv /tmp/config.yaml /opt/livekit-server/config.yaml",
      # "sudo chown root:root /opt/livekit-server/config.yaml",
      "sudo systemctl enable docker.livekit-server@${local.livekit_server_version}",
      "sudo systemctl start docker.livekit-server@${local.livekit_server_version}",
    ]
  }

}
