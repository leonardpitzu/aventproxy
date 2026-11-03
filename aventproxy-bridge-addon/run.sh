#!/usr/bin/env sh
set -e

# The Philips Avent integration sets this app's options through the
# Supervisor, which writes them here before every start. A change arrives as an
# app restart, so the file never changes under a running bridge.
CONFIG_PATH="/data/options.json"

echo "=============================="
echo "Philips Avent WebRTC Bridge"
echo "Config: $CONFIG_PATH"
echo "=============================="

# Restart the bridge here rather than exiting the container on a crash. The old
# version relied on the supervisor to bring the app back; after two Home
# Assistant restarts in quick succession it did not, and the app sat stopped
# with no video and nothing in the integration log to explain it (issue #73).
#
# A stop request must end the loop instead. `wait` returns as soon as the
# trapped signal arrives, before the bridge has exited, so without the flag the
# loop took the TERM for a crash, started the bridge again, and the Supervisor
# had to kill the container when its stop timeout ran out.
BRIDGE_PID=""
STOPPING=""
trap 'STOPPING=1; kill -TERM "$BRIDGE_PID" 2>/dev/null || true' TERM INT

while [ -z "$STOPPING" ]; do
    avent-webrtc-bridge addon --config "$CONFIG_PATH" &
    BRIDGE_PID=$!
    echo "Bridge started (pid $BRIDGE_PID)"

    set +e
    wait "$BRIDGE_PID"
    BRIDGE_EXIT=$?
    if [ -n "$STOPPING" ]; then
        wait "$BRIDGE_PID"
        echo "Bridge stopped"
        exit 0
    fi
    set -e

    if [ "$BRIDGE_EXIT" -eq 0 ]; then
        echo "Bridge shut down cleanly"
        exit 0
    fi

    echo "Bridge exited with status $BRIDGE_EXIT, restarting in 5s..."
    sleep 5
done
echo "Bridge stopped"
