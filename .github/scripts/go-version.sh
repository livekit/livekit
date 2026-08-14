#!/usr/bin/env bash
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

#
# Print the Go toolchain version pinned by a Dockerfile's golang builder image.
#
#   usage: .github/scripts/go-version.sh [dockerfile]   (default: Dockerfile)
#
# The pinned `golang:X.Y.Z-alpineA.B@sha256:...` builder image is the single source
# of truth, and Renovate keeps it current via an approvable PR. Each workflow reads
# the version out of the Dockerfile it builds against (setup-go cannot parse a
# Dockerfile itself), so a job's Go runtime always matches the image it produces.
#
# Deliberately NOT derived from go.mod's `go` directive: that is the module
# minimum, owned by the toolchain and by dependency requirements, and the go
# command may rewrite it to a bare major.minor.
set -euo pipefail

dockerfile="${1:-Dockerfile}"

if [ ! -f "$dockerfile" ]; then
  echo "$dockerfile: no such file" >&2
  exit 1
fi

version=$(sed -nE 's/^FROM[[:space:]]+golang:([0-9]+\.[0-9]+\.[0-9]+)[^[:space:]]*.*/\1/p' "$dockerfile" | sort -u)

if [ -z "$version" ]; then
  echo "$dockerfile: no pinned 'FROM golang:X.Y.Z-...' base image found" >&2
  exit 1
fi

if [ "$(printf '%s\n' "$version" | wc -l)" -ne 1 ]; then
  echo "$dockerfile pins more than one Go version: $(echo $version)" >&2
  exit 1
fi

echo "$version"
