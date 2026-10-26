# Copyright 2023 LiveKit, Inc.
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

# Pinned by digest so the build is reproducible even if the tag is republished.
# The tag is kept alongside it for readability; Renovate updates both together.
# This image is also the single source of truth for the Go toolchain: CI reads the
# version out of this line (see .github/scripts/go-version.sh) so tests and images
# always run the same runtime.
FROM golang:1.27.3-alpine3.24@sha256:f2c872bcee949b7be9c490efd02b48a8a2fbf947cdf533f1c58525ec9d8170fd AS builder

ARG TARGETPLATFORM
ARG TARGETARCH
RUN echo building for "$TARGETPLATFORM"

WORKDIR /workspace

# Build with exactly the toolchain in this image; fail (don't silently download)
# if go.mod ever requires a newer version, so the pinned image stays authoritative.
ENV GOTOOLCHAIN=local

# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY cmd/ cmd/
COPY pkg/ pkg/
COPY test/ test/
COPY version/ version/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH GO111MODULE=on go build -a -o livekit-server ./cmd/server

FROM alpine:3.24.3@sha256:c6b073f8e9e58d881f6ffba6ebf3b9ebd60b9e7d6e8337018c0e68bb5c356873

# Pull the latest security patches for base packages within the pinned Alpine
# release. The digest above is only refreshed by a Renovate PR, so without this
# an image can ship base-package CVEs that Alpine has already fixed.
# NOTE: this relies on the build starting with a cold layer cache, which is true
# today because the release workflow configures no buildx cache. If cache-from/
# cache-to is ever added, this layer needs a cache-busting ARG (as ../cloud does
# with SECURITY_REFRESH) or it will be served stale.
RUN apk upgrade --no-cache

COPY --from=builder /workspace/livekit-server /livekit-server

# Run the binary.
ENTRYPOINT ["/livekit-server"]
