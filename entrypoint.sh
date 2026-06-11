#!/bin/sh
# entrypoint.sh — translate environment variables into ubersdr_lightning flags
#
# Environment variables:
#   UBERSDR_URL       UberSDR WebSocket URL (default: ws://ubersdr:8080/ws)
#   WEB_PORT          Port for the web UI server (default: 6097)
#   CENTRE_HZ         IQ centre frequency in Hz (default: 20000)
#   IIR_ALPHA         IIR noise floor alpha (default: 0.9999)
#   THRESHOLD_RATIO   Trigger threshold ratio (default: 2.0)

set -e

args=""

[ -n "$UBERSDR_URL"      ] && args="$args -url $UBERSDR_URL"
[ -n "$CENTRE_HZ"        ] && args="$args -centre-hz $CENTRE_HZ"
[ -n "$IIR_ALPHA"        ] && args="$args -iir-alpha $IIR_ALPHA"
[ -n "$THRESHOLD_RATIO"  ] && args="$args -threshold $THRESHOLD_RATIO"

# WEB_PORT → -listen :<port>
if [ -n "$WEB_PORT" ]; then
    args="$args -listen :$WEB_PORT"
else
    args="$args -listen :6097"
fi

# Append any CLI args passed directly to the container
# shellcheck disable=SC2086
exec /usr/local/bin/ubersdr_lightning $args "$@"
