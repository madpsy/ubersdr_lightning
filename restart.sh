#!/usr/bin/env bash
# restart.sh — restart the ubersdr_lightning service

set -euo pipefail

INSTALL_DIR="${HOME}/ubersdr/lightning"

cd "${INSTALL_DIR}"
echo "Stopping ubersdr_lightning..."
docker compose down
echo "Starting ubersdr_lightning..."
docker compose up -d --remove-orphans
echo "Done."
echo "  View logs : docker compose logs -f"
echo "  Web UI    : http://localhost:6097"
