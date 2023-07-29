#!/usr/bin/env bash
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

# LiveKit install script for Linux

set -u
set -o errtrace
set -o errexit
set -o pipefail

REPO="livekit"
INSTALL_PATH="/usr/local/bin"

log()  { printf "%b\n" "$*"; }
abort() {
  printf "%s\n" "$@" >&2
  exit 1
}

# returns the latest version according to GH
# i.e. 1.0.0
get_latest_version()
{
  latest_version=$(curl -s https://api.github.com/repos/livekit/$REPO/releases/latest | grep -oP '"tarball_url": ".*/tarball/v\K([^/]*)(?=")')
  printf "%s" "$latest_version"
}

# Ensure bash is used
if [ -z "${BASH_VERSION:-}" ]
then
  abort "This script requires bash"
fi

# Check if $INSTALL_PATH exists
if [ ! -d ${INSTALL_PATH} ]
then
  abort "Could not install, ${INSTALL_PATH} doesn't exist"
fi

# Needs SUDO if no permissions to write
SUDO_PREFIX=""
if [ ! -w ${INSTALL_PATH} ]
then
  SUDO_PREFIX="sudo"
  log "sudo is required to install to ${INSTALL_PATH}"
fi

# Check cURL is installed
if ! command -v curl >/dev/null
then
  abort "cURL is required and is not found"
fi

# OS check
OS="$(uname)"
if [[ "${OS}" == "Darwin" ]]
then
  abort "Installer not supported on MacOS, please install using Homebrew."
elif [[ "${OS}" != "Linux" ]]
then
  abort "Installer is only supported on Linux."
fi

ARCH="$(uname -m)"

# fix arch on linux
if [[ "${ARCH}" == "aarch64" ]]
then
  ARCH="arm64"
elif [[ "${ARCH}" == "x86_64" ]]
then
  ARCH="amd64"
fi

VERSION=$(get_latest_version)
ARCHIVE_URL="https://github.com/livekit/$REPO/releases/download/v${VERSION}/${REPO}_${VERSION}_linux_${ARCH}.tar.gz"

# Ensure version follows SemVer
if ! [[ "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]
then
  abort "Invalid version: ${VERSION}"
fi

log "Installing ${REPO} ${VERSION}"
log "Downloading from ${ARCHIVE_URL}..."

curl -s -L "${ARCHIVE_URL}" | ${SUDO_PREFIX} tar xzf - -C "${INSTALL_PATH}" --wildcards --no-anchored "$REPO*"

log "\nlivekit-server is installed to $INSTALL_PATH\n"
