# LiveKit Server Deployment

Most options require that you already have a LiveKit Server config.yaml file with a valid self-generated API key. 

## Cloud Init

### AWS EC2

LiveKit Server can be deployed with either Amazon Linux 2 or Ubuntu. Use the  corresponding to your desired platform:
	- For Amazon Linux 2, use [cloud-init.amzn2.yaml](https://raw.githubusercontent.com/livekit/livekit-server/master/deploy/cloud-init.amzn2.yaml)
	- For Ubuntu, use [cloud-init.ubuntu.yaml](https://raw.githubusercontent.com/livekit/livekit-server/master/deploy/cloud-init.ubuntu.yaml)

1. Download one of the above `cloud-init.<platform>.yaml` files, and edit it to include your LiveKit Server configuration. Your `config.yaml` goes under the first `write_files` item `contents`.
2. Open the [AWS Console](https://console.aws.amazon.com/ec2) and navigate to EC2.
3. Launch an instance.
4. Choose the latest "Amazon Linux 2" or AMI.
5. In the "Step 3: Configure Instance Details" screen, copy and paste text from your modified `cloud-init.yaml` file from step 1 into the "User data" field, under the "Advanced Details" section.
6. Launch your instance as usual.

### Digital Ocean
TODO


## Custom Built Cloud Image

### AWS EC2

#### Amazon Linux 2
1. [Install Packer](https://learn.hashicorp.com/tutorials/packer/get-started-install-cli)
2. (Optional) Edit [livekit-server/deploy/config.pkr.hcl](https://raw.githubusercontent.com/livekit/livekit-server/master/deploy/config.pkr.hcl)
3. Build your AMI:
```
cd livekit-server/deploy
packer build .
```
4. Edit [cloud-init.lk-image.yaml](https://raw.githubusercontent.com/livekit/livekit-server/master/deploy/cloud-init.lk-image.yaml) with your LiveKit Server configuration.
5. Launch your EC2 instance following the instructions in [Cloud Init AWS EC2 Amazon Linux 2](#amazon-linux-2)

#### Ubuntu
TODO

### Digital Ocean
TODO

## Kubernetes Helm Chart

See https://github.com/livekit/livekit-helm