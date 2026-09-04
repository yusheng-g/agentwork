#!/bin/sh
# Builds the agentwork CLI and daemon with one shared version stamp.
#
#   AGENTWORK_COMPILE_VERSION=0.0.1-beta.2 ./build.sh
#
# The env var overrides the default (0.0.1-beta.1). Both binaries MUST be
# built with the same stamp: the register-time version check between the
# CLI and the daemon warns on a mismatch, and mixed stamps are the symptom
# of a mixed deploy.
#
# Event tracking (task-count analytics) is ldflags-injected. Empty =
# disabled — the endpoint URL and AppID are NOT hardcoded here; they are
# injected via the EVENT_POST_URL / EVENT_APP_ID env vars, typically set as
# CI pipeline variables (not in the repo). A plain ./build.sh or `go build`
# without these vars produces a disabled binary that makes no HTTP calls.
#   EVENT_POST_URL=http://127.0.0.1:9999/track EVENT_APP_ID=mock ./build.sh  # enable
#   EVENT_POST_URL= ./build.sh                                               # disable
set -e
cd "$(dirname "$0")"

VERSION="${AGENTWORK_COMPILE_VERSION:-0.0.1-beta.1}"
LDFLAGS="-X main.cliVersion=$VERSION \
-X github.com/eushing/agentwork/internal/version.DaemonVersion=$VERSION \
-X github.com/eushing/agentwork/internal/track.EventPostUrl=${EVENT_POST_URL:-} \
-X github.com/eushing/agentwork/internal/track.AppID=${EVENT_APP_ID:-} \
-X github.com/eushing/agentwork/internal/track.EventSkipVerify=${SKIP_VERIFY:-false}"

mkdir -p build
go build -ldflags "$LDFLAGS" -o build/agentwork ./cmd/agentwork-cli
go build -ldflags "$LDFLAGS" -o build/agentwork-daemon ./cmd/agentwork-daemon
echo "built build/agentwork (CLI) + build/agentwork-daemon v$VERSION"
