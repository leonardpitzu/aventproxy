# Philips Avent WebRTC Bridge

Serves the video of your Philips Avent baby monitors over RTSP, for the
Philips Avent integration's camera entities.

## Setup

1. Install this app and start it.
2. Add the **Philips Avent Baby Monitor** integration under
   **Settings → Devices & services**.

There is nothing to fill in on the **Configuration** tab. The integration sets
this app's options itself, through the Supervisor: the account session, each
camera's local key and password, and the RTSP port. It restarts the app when
they change, and only then. Anything edited on that tab by hand is overwritten
the next time the integration starts.

Until the integration has set the options, the app logs that it is waiting and
serves nothing.

Removing the integration stops the app and clears its options.
