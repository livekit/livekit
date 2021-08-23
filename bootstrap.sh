#!/bin/bash

if ! command -v protoc &> /dev/null
then
  echo "protoc is required and not found. please install"
  exit 1
fi

if ! command -v mage &> /dev/null
then
  pushd /tmp
  git clone https://github.com/magefile/mage
  cd mage
  go run bootstrap.go
  rm -rf /tmp/mage
  popd
fi

if ! command -v mage &> /dev/null
then
  echo "Ensure `go env GOPATH`/bin is in your \$PATH"
  exit 1
fi

go mod download

GO_VERSION=`go version | { read _ _ v _; echo ${v#go}; }`
GO_TARGET_VERSION=1.17

function version { echo "$@" | awk -F. '{ printf("%d%03d%03d%03d\n", $1,$2,$3,$4); }'; }

if [ $(version $GO_VERSION) -ge $(version $GO_TARGET_VERSION) ];
  then
    go install github.com/twitchtv/twirp/protoc-gen-twirp@v8.1.0
    go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.27.1
  else
    go get -u github.com/twitchtv/twirp/protoc-gen-twirp@v8.1.0
    go get -u google.golang.org/protobuf/cmd/protoc-gen-go@v1.27.1
fi