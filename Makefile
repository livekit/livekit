dockerbuild: 
	docker build -t 012634413971.dkr.ecr.ap-northeast-2.amazonaws.com/lms/livekit --build-arg servicename="livekit" . --progress=plain --no-cache
dockerpush:
	# 로그인
	aws ecr get-login-password --region ap-northeast-2 | docker login --username AWS --password-stdin "$$(aws sts get-caller-identity --query Account --output text).dkr.ecr.ap-northeast-2.amazonaws.com"
	
	# docker push
	docker push 012634413971.dkr.ecr.ap-northeast-2.amazonaws.com/lms/livekit:latest