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
#
# Pinned by digest so the build is reproducible even if the tag is republished.
# The tag is kept alongside it for readability; Renovate updates both together.
FROM golang:1.27.3-alpine3.24@sha256:f2c872bcee949b7be9c490efd02b48a8a2fbf947cdf533f1c58525ec9d8170fd AS builder

ARG TARGETARCH

WORKDIR /workspace

# Build with exactly the toolchain in this image; fail (don't silently download)
# if go.mod ever requires a newer version, so the pinned image stays authoritative.
ENV GOTOOLCHAIN=local

COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY cmd/ cmd/
COPY pkg/ pkg/
COPY version/ version/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH GO111MODULE=on go build -a -o livekit-test-server ./cmd/test-server

FROM alpine:3.24.3@sha256:c6b073f8e9e58d881f6ffba6ebf3b9ebd60b9e7d6e8337018c0e68bb5c356873

# Pull the latest security patches for base packages within the pinned Alpine
# release; the digest above is only refreshed by a Renovate PR. See the note in
# the repo-root Dockerfile about the cold-cache assumption.
RUN apk upgrade --no-cache

COPY --from=builder /workspace/livekit-test-server /livekit-test-server

# region-0 (primary, 9999) + 3 fallback regions
EXPOSE 9999 10000 10001 10002

ENTRYPOINT ["/livekit-test-server"]
