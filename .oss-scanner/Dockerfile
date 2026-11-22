# Copyright 2026 LiveKit, Inc.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Build image for Anthropic's OSS Scanner. Keep the Go version at or above go.mod's: GOTOOLCHAIN=local won't fetch one.
FROM golang:1.26-bookworm

ENV DEBIAN_FRONTEND=noninteractive \
    GOTOOLCHAIN=local

RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential ca-certificates git redis-server \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY . /src

RUN go mod download && go mod verify
RUN go build ./... && go build -o /usr/local/bin/livekit-server ./cmd/server

# Tests use a local redis on :6379; pkg/service also wants a docker daemon unless -docker=false.
RUN redis-server --daemonize yes \
    && go test -count=1 -timeout 20m $(go list ./... | grep -v '/pkg/service$') \
    && go test -count=1 -timeout 10m ./pkg/service -docker=false \
    || echo 'WARNING: baseline tests failed'

ENV GOPROXY=off \
    GOSUMDB=off

CMD ["bash"]
