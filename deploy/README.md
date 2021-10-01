# LiveKit Server Deployment

This document covers setting up LiveKit server. You should already have a LiveKit server config.yaml file, with a valid self-generated API key by following the instructions in the top level [README.md](https://github.com/livekit/livekit-server/blob/master/README.md). If you need SSL termination, you will also need an SSL certificate and key.

## Deployment
You have the choice of deploying LiveKit using [Cloud Init](#cloud-init) or with a [Custom Built Cloud Image](#custom-built-cloud-image). If you prefer to deploy LiveKit on Kubernetes, see https://github.com/livekit/livekit-helm instead.

### Cloud Init
You can deploy a LiveKit server using a vanilla Linux image and Cloud Init configuration file.

#### AWS EC2

LiveKit Server can be deployed with either Amazon Linux 2 or Ubuntu. Use the `cloud-init.<platform>.yaml` file corresponding to your desired platform:
  - For Amazon Linux 2, use [cloud-init.amzn2.yaml](https://raw.githubusercontent.com/livekit/livekit-server/master/deploy/cloud-init.amzn2.yaml)
  - For Ubuntu, use [cloud-init.ubuntu.yaml](https://raw.githubusercontent.com/livekit/livekit-server/master/deploy/cloud-init.ubuntu.yaml)

Steps: 
  1. Download one of the above `cloud-init.<platform>.yaml` files, and edit it to include your LiveKit Server configuration. Your `config.yaml` goes under the first `write_files` item `contents`.
  2. Open the [AWS Console](https://console.aws.amazon.com/ec2) and navigate to EC2.
  3. Launch an instance.
  4. Choose the latest "Amazon Linux 2" AMI.
  5. In the "Step 3: Configure Instance Details" screen, copy and paste text from your modified `cloud-init.yaml` file from step 1 into the "User data" field, under the "Advanced Details" section.
  6. Launch your instance as usual.


### Custom Built Cloud Image
You can build your own custom cloud image, and bake in any configuration you like.

#### AWS EC2

##### Amazon Linux 2
  1. [Install Packer](https://learn.hashicorp.com/tutorials/packer/get-started-install-cli)
  2. (Optional) Edit [livekit-server/deploy/config.pkr.hcl](https://raw.githubusercontent.com/livekit/livekit-server/master/deploy/config.pkr.hcl)
  3. Build your AMI:
```
cd livekit-server/deploy
packer build .
```
  4. Edit [cloud-init.lk-image.yaml](https://raw.githubusercontent.com/livekit/livekit-server/master/deploy/cloud-init.lk-image.yaml) with your LiveKit Server configuration.
  5. Open the [AWS Console](https://console.aws.amazon.com/ec2) and navigate to EC2.
  6. Launch an instance.
  7. Choose custom built AMI.
  8. In the "Step 3: Configure Instance Details" screen, copy and paste text from your modified `cloud-init.yaml` file from step 1 into the "User data" field, under the "Advanced Details" section.
  9. Launch your instance as usual.

## Firewall
After you have [deployed](#deployment) LiveKit using one of the methods above, make sure your AWS Security Group is configured to allow the following ports to your LiveKit Instance:
  - TPC 443  - SSL terminated WebSocket
  - TCP 7880 - Plaintext WebSocket
  - TCP 7881 - RTC TCP
  - UDP 7882 - RTC UDP