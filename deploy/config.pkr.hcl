packer {
  required_plugins {
    amazon = {
      version = ">= 0.0.2"
      source  = "github.com/hashicorp/amazon"
    }

    # # TODO: build a LiveKit image on DigitalOcean
    # digitalocean = {
    #   version = ">= 1.0.0"
    #   source  = "github.com/hashicorp/digitalocean"
    # }
  }
}


# # Uncomment when creating a custom image without cloud-init
# locals {
#   livekit_version = "v0.13"
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


  provisioner "file" {
    source      = "docker.livekit-server@.service"
    destination = "/tmp/docker.livekit-server@.service"
  }

  provisioner "file" {
    source      = "livekit-nginx.conf"
    destination = "/tmp/livekit-livekit.conf"
  }

  # # Uncomment when creating a custom image without cloud-init
  # provisioner "file" {
  #   source      = "config.yaml" # LiveKit config
  #   destination = "/tmp/config.yaml"
  # }
  #
  # provisioner "file" {
  #   source      = "server.crt" # SSL cert
  #   destination = "/tmp/server.crt"
  # }
  #
  # provisioner "file" {
  #   source      = "server.key" # SSL key
  #   destination = "/tmp/server.key"
  # }


  provisioner "shell" {
    environment_vars = []
    inline = [
      # docker
      "sudo yum update -y",
      "sudo yum install -y docker",
      "sudo systemctl enable docker",

      # livekit
      "sudo mv /tmp/docker.livekit-server@.service /etc/systemd/system/docker.livekit-server@.service",
      "sudo chown root:root /etc/systemd/system/docker.livekit-server@.service",
      "sudo mkdir -p /opt/livekit-server/ssl",

      # nginx
      "sudo amazon-linux-extras install -y nginx1",
      "sudo mv /tmp/livekit-nginx.conf /etc/nginx/conf.d/livekit.conf"
      "sudo systemctl enable nginx",

      # # Uncomment when creating a custom image without cloud-init
      # "sudo mv /tmp/config.yaml /opt/livekit-server/config.yaml",
      # "sudo chown root:root /opt/livekit-server/config.yaml",
      # "sudo systemctl enable docker.livekit-server@${local.livekit_version}",
      # "sudo systemctl start docker.livekit-server@${local.livekit_version}",
      #
      # "sudo mv /tmp/server.crt /opt/livekit-server/ssl/server.crt",
      # "sudo mv /tmp/server.key /opt/livekit-server/ssl/server.key",
      # "sudo chown root:root /opt/livekit-server/ssl/*"
      # "sudo chown 600 /opt/livekit-server/ssl/*"
    ]
  }

}
