#!/usr/bin/env bash
# ──────────────────────────────────────────────
# Local render script for data-disk-fn
# ──────────────────────────────────────────────
# Usage:
#   ./render.sh                          # Render with default claim
#   ./render.sh claim-multi-disk.yaml    # Render with custom claim
#
# Prerequisites:
#   1. Crossplane CLI installed:
#        brew install crossplane/tap/crossplane
#   2. Docker installed and running (render uses Docker by default)
#   3. Go installed (for building the function)
# ──────────────────────────────────────────────

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

COMPOSITION="${REPO_ROOT}/composition.yaml"
CLAIM="${1:-${REPO_ROOT}/claim.yaml}"
FUNCTION="${SCRIPT_DIR}/function.yaml"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log() { echo -e "${GREEN}[render]${NC} $*"; }
warn() { echo -e "${YELLOW}[render]${NC} $*"; }
err()  { echo -e "${RED}[render]${NC} $*" >&2; }

# ── Step 1: Verify prerequisites ──
log "Checking prerequisites..."

if ! command -v crossplane &>/dev/null; then
    err "Crossplane CLI not found. Install with: brew install crossplane/tap/crossplane"
    exit 1
fi

if ! command -v docker &>/dev/null; then
    err "Docker not found. Install Docker Desktop: https://www.docker.com/products/docker-desktop/"
    exit 1
fi

if ! docker info &>/dev/null; then
    err "Docker is not running. Start Docker Desktop first."
    exit 1
fi

if ! command -v go &>/dev/null; then
    err "Go not found. Install Go first."
    exit 1
fi

if [[ ! -f "$COMPOSITION" ]]; then
    err "Composition not found at: $COMPOSITION"
    exit 1
fi

if [[ ! -f "$CLAIM" ]]; then
    err "Claim not found at: $CLAIM"
    exit 1
fi

if [[ ! -f "$FUNCTION" ]]; then
    err "Function CRD not found at: $FUNCTION"
    exit 1
fi

# ── Step 2: Build the function binary ──
log "Building data-disk-fn..."
cd "$SCRIPT_DIR"
go build -o fn .
log "Built: ${SCRIPT_DIR}/fn"

# ── Step 3: Start the function in the background ──
log "Starting function on :9443..."
PIDS=$(lsof -ti :9443 2>/dev/null || true)
if [[ -n "$PIDS" ]]; then
    warn "Port 9443 already in use (PIDs: $PIDS). Killing old processes."
    kill $PIDS 2>/dev/null || true
    sleep 1
fi

"$SCRIPT_DIR/fn" --insecure --address :9443 &
FN_PID=$!
trap "kill $FN_PID 2>/dev/null; wait $FN_PID 2>/dev/null" EXIT

# Wait for function to be ready
log "Waiting for function to start..."
for i in $(seq 1 10); do
    if lsof -ti :9443 &>/dev/null; then
        log "Function is ready (PID: $FN_PID)"
        break
    fi
    if [[ $i -eq 10 ]]; then
        err "Function failed to start on :9443"
        exit 1
    fi
    sleep 1
done

# ── Step 4: Render the composition ──
# Order: <composite-resource> <composition> [<functions>]
log "Rendering composition..."
log "  Claim:       $CLAIM"
log "  Composition: $COMPOSITION"
log "  Function:    $FUNCTION"
echo ""

crossplane composition render \
    "$CLAIM" \
    "$COMPOSITION" \
    "$FUNCTION" 2>&1

RENDER_EXIT=$?

echo ""
if [[ $RENDER_EXIT -eq 0 ]]; then
    log "Render completed successfully!"
else
    err "Render failed with exit code $RENDER_EXIT"
fi

exit $RENDER_EXIT
