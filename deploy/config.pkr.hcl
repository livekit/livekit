packer {
  required_plugins {
    amazon = {
      version = ">= 0.0.2"
      source  = "github.com/hashicorp/amazon"
    }
    # digitalocean = {
    #   version = ">= 1.0.0"
    #   source  = "github.com/hashicorp/digitalocean"
    # }
  }
}


# # Uncomment if not using cloud-init
# locals {
#   livekit_server_version = "v0.13"
# }

source "amazon-ebs" "amzn2" {
  ami_name      = "livekit-amzn2-{{timestamp}}"
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
  name = "livekit-centos"
  sources = [
    "source.amazon-ebs.amzn2"
  ]

  # # Uncomment if not using cloud-init
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

      # # Uncomment if not using cloud-init
      # "sudo mv /tmp/config.yaml /opt/livekit-server/config.yaml",
      # "sudo chown root:root /opt/livekit-server/config.yaml",
      # "sudo systemctl enable docker.livekit-server@${local.livekit_server_version}",
      # "sudo systemctl start docker.livekit-server@${local.livekit_server_version}",
    ]
  }

}
