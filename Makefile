
# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

server:
	go build -i -o livekit-server

GO_TARGET=proto
proto: protoc goproto
	@{ \
	protoc --go_out=$(GO_TARGET) --go-grpc_out=$(GO_TARGET) \
    	--go_opt=paths=source_relative \
    	--go-grpc_opt=paths=source_relative \
    	--plugin=$(PROTOC_GEN_GO) \
    	-I=proto \
    	proto/*.proto ;\
    }

protoc:
ifeq (, $(shell which protoc))
	echo "protoc is required, and is not installed"
endif

goproto:
ifeq (, $(shell which protoc-gen-go-grpc))
	@{ \
	echo "installing go protobuf plugin" ;\
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc ;\
	}
PROTOC_GEN_GO=$(GOBIN)/protoc-gen-go-grpc
else
PROTOC_GEN_GO=$(shell which protoc-gen-go-grpc)
endif
