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

go get -u github.com/twitchtv/twirp/protoc-gen-twirp
go get -u google.golang.org/protobuf/cmd/protoc-gen-go

go mod download
