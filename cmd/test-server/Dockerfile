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

# Mock LiveKit API server for SDK conformance testing. Build from the repo
# root:  docker build -f cmd/test-server/Dockerfile -t livekit/test-server .
FROM golang:1.26-alpine AS builder

ARG TARGETARCH

WORKDIR /workspace

COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY cmd/ cmd/
COPY pkg/ pkg/
COPY version/ version/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH GO111MODULE=on go build -a -o livekit-test-server ./cmd/test-server

FROM alpine

COPY --from=builder /workspace/livekit-test-server /livekit-test-server

# region-0 (primary, 9999) + 3 fallback regions
EXPOSE 9999 10000 10001 10002

ENTRYPOINT ["/livekit-test-server"]
