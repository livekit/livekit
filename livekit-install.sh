#!/usr/bin/env bash
# LiveKit install script for Linux

set -u
set -o errtrace
set -o errexit
set -o pipefail

REPO="livekit-cli"
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

ARCH="$(/usr/bin/uname -m)"

# fix arch on linux
if [[ "${ARCH}" == "aarch64" ]]
then
  ARCH="arm64"
fi

VERSION=$(get_latest_version)
ARCHIVE_URL="https://github.com/livekit/$REPO/releases/download/v${VERSION}/${REPO}_${VERSION}_linux_${ARCH}.tar.gz"

log "Installing ${VERSION}"
log "fetching from $ARCHIVE_URL"

curl -s -L "${ARCHIVE_URL}" | tar xzf - -C "${INSTALL_PATH}" --wildcards --no-anchored "$REPO*"

log "\n$REPO is installed to $INSTALL_PATH\n"
