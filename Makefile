.MAIN: build
dir = $(shell pwd)

build:
	docker build -t livekit -f Dockerfile .

run:
	docker stop livelit-letsencrypt || true
	docker rm livelit-letsencrypt || true
	docker run --rm \
	--name livelit-letsencrypt \
	-p 7880:7880 \
	-p 7881:7881 \
	-p 7882:7882/udp \
	-v $(shell pwd)/livekit-letsencrypt.yaml:/livekit.yaml \
	-v /tmp:/tmp \
	-e "NODEIP=75.59.238.123" \
	-e "CONFIG=/livekit.yaml" \
	livekit &
