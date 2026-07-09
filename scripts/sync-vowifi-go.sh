#!/usr/bin/env bash
# Fetch private vowifi-go@v1.1.2 (runtimecore + imscore) and switch off the
# local replace directive. Requires a GitHub PAT with repo read on iniwex5/*.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TARGET="${VOHIVE_VOWIFI_GO_DIR:-/root/vowifi-go-v1.1.2}"
TAG="${VOHIVE_VOWIFI_GO_TAG:-v1.1.2}"
PAT="${GH_PAT:-${GITHUB_TOKEN:-}}"

if [[ -z "${PAT}" ]]; then
  echo "GH_PAT or GITHUB_TOKEN is required to clone github.com/iniwex5/vowifi-go" >&2
  exit 1
fi

if [[ -d "${TARGET}/.git" ]]; then
  git -C "${TARGET}" fetch --tags origin
  git -C "${TARGET}" checkout "${TAG}"
else
  rm -rf "${TARGET}"
  git clone --branch "${TAG}" --depth 1 \
    "https://x-access-token:${PAT}@github.com/iniwex5/vowifi-go.git" \
    "${TARGET}"
fi

echo "vowifi-go ${TAG} ready at ${TARGET}"
echo "Next: in ${ROOT}/go.mod set"
echo "  replace github.com/iniwex5/vowifi-go => ${TARGET}"
echo "then: cd ${ROOT} && go build -o /opt/vohive/bin/vohive ./cmd/vohive"