#!/bin/sh

cd /tmp
git clone https://github.com/magefile/mage
cd mage
go run bootstrap.go
rm -rf mage

if ! command -v mage &> /dev/null
then
  echo "Ensure ${GOPATH}/bin is in your \$PATH"
fi
