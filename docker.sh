#!/usr/bin/env bash
# docker.sh — build the ubersdr_lightning Docker image
#
# All binaries are built from source inside the Docker image.
# No host binaries are required.
#
# Usage:
#   ./docker.sh [build|push|run|arm64]
#
#   build  — build the image for linux/amd64 (default, local load)
#   arm64  — build the image for linux/arm64 (Raspberry Pi, Apple Silicon, etc.)
#   push   — build multi-platform manifest (amd64 + arm64) via buildx and push
#   run    — run the image locally (set env vars below)
#
# Environment variables (build):
#   IMAGE      Docker image name/tag   (default: madpsy/ubersdr_lightning:latest)
#   PLATFORM   Docker --platform flag  (default: linux/amd64)
#   BUILDER    buildx builder name     (default: ubersdr_lightning_builder)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

IMAGE="${IMAGE:-madpsy/ubersdr_lightning:latest}"
PLATFORM="${PLATFORM:-linux/amd64}"
BUILDER="${BUILDER:-ubersdr_lightning_builder}"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

die() { echo "error: $*" >&2; exit 1; }

check_deps() {
    command -v docker >/dev/null || die "docker not found in PATH"
}

# Ensure a buildx builder that supports multi-platform builds exists.
# Uses the existing builder if already present; creates one otherwise.
ensure_builder() {
    if ! docker buildx inspect "$BUILDER" &>/dev/null; then
        echo "Creating buildx builder '$BUILDER'..."
        docker buildx create --name "$BUILDER" --driver docker-container --bootstrap
    else
        echo "Using existing buildx builder '$BUILDER'."
    fi
}

stage_context() {
    TMPCTX="$(mktemp -d)"
    # shellcheck disable=SC2064
    trap 'rm -rf "$TMPCTX"' EXIT

    echo "Staging build context in $TMPCTX..."
    rsync -a --exclude='.git' \
              --exclude='recordings' \
              --exclude='data' \
              --exclude='ubersdr_lightning' \
              "$SCRIPT_DIR/" "$TMPCTX/"
}

build() {
    check_deps
    stage_context

    echo "Building image $IMAGE (platform=$PLATFORM)..."
    docker build \
        --platform "$PLATFORM" \
        --tag "$IMAGE" \
        "$TMPCTX"

    echo "Built: $IMAGE"
}

push() {
    check_deps
    ensure_builder
    stage_context

    local platforms="linux/amd64,linux/arm64"
    echo "Building and pushing multi-platform image $IMAGE (platforms=$platforms)..."
    docker buildx build \
        --builder "$BUILDER" \
        --platform "$platforms" \
        --tag "$IMAGE" \
        --push \
        "$TMPCTX"

    echo "Pushed multi-platform manifest: $IMAGE"
    echo "Committing and pushing git repository..."
    git add -A
    git diff --cached --quiet || git commit -m "Release $IMAGE"
    git push
}

run_image() {
    local args=()

    [[ -n "${UBERSDR_URL:-}"      ]] && args+=(-e "UBERSDR_URL=$UBERSDR_URL")
    [[ -n "${UBERSDR_PASS:-}"     ]] && args+=(-e "UBERSDR_PASS=$UBERSDR_PASS")
    [[ -n "${WEB_PORT:-}"         ]] && args+=(-e "WEB_PORT=$WEB_PORT")
    [[ -n "${CENTRE_HZ:-}"        ]] && args+=(-e "CENTRE_HZ=$CENTRE_HZ")
    [[ -n "${IIR_ALPHA:-}"        ]] && args+=(-e "IIR_ALPHA=$IIR_ALPHA")
    [[ -n "${THRESHOLD_RATIO:-}"  ]] && args+=(-e "THRESHOLD_RATIO=$THRESHOLD_RATIO")

    docker run --rm -it \
        --platform "$PLATFORM" \
        -p "${WEB_PORT:-6097}:${WEB_PORT:-6097}" \
        "${args[@]}" \
        "$IMAGE" \
        "$@"
}

# ---------------------------------------------------------------------------
# Environment variable reference (for docker run -e ...)
# ---------------------------------------------------------------------------
#
#   UBERSDR_URL      UberSDR WebSocket URL (default: ws://ubersdr:8080/ws)
#   UBERSDR_PASS     UberSDR bypass password (optional)
#   WEB_PORT         Web UI port (default: 6097)
#   CENTRE_HZ        IQ centre frequency in Hz (default: 20000)
#   IIR_ALPHA        IIR noise floor alpha (default: 0.9999)
#   THRESHOLD_RATIO  Trigger threshold ratio (default: 2.0)

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

case "${1:-build}" in
    build) build ;;
    arm64) PLATFORM=linux/arm64 build ;;
    push)  push  ;;
    run)   shift; run_image "$@" ;;
    *)
        echo "Usage: $0 [build|arm64|push|run [args...]]" >&2
        exit 1
        ;;
esac
