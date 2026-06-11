#!/usr/bin/env bash
# stop.sh — stop the ubersdr_lightning service

set -euo pipefail

INSTALL_DIR="${HOME}/ubersdr/lightning"

cd "${INSTALL_DIR}"
echo "Stopping ubersdr_lightning..."
docker compose down
echo "Done."
