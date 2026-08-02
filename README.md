# Philips Avent Baby Monitor for Home Assistant

<p align="center">
  <img src="custom_components/philips_avent/brand/logo.png" alt="Philips AVENT" width="300">
</p>

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![HACS](https://img.shields.io/badge/HACS-Custom-41BDF5.svg)](https://github.com/hacs/integration)

A custom [Home Assistant](https://www.home-assistant.io/) integration for [Philips Avent](https://www.philips.co.uk/c-m-mo/baby-monitors) SCD9xx baby monitors - live video, room temperature, night light, lullabies and motion/sound alerts.

Video and control run **over the local network**, with the Tuya cloud used only once at setup to fetch the per-device keys. When the local path is unavailable the integration falls back to the cloud automatically, so a camera on another subnet still works.

> This is a fork of [thekoma/aventproxy](https://github.com/thekoma/aventproxy).

## Features

| Feature | Entity | Description |
|---|---|---|
| Live video | `camera` | 1080p H.264, served as RTSP by the bridge add-on |
| Temperature | `sensor` | Room temperature from the built-in sensor |
| WiFi signal | `sensor` | Monitor's RSSI |
| Night light | `switch` + `number` | On/off plus brightness 1-100 % |
| Lullabies | `button` + `select` + `number` | Play/pause/stop/next/prev, track, timer, volume |
| Motion alert | `switch` | Enable motion detection on the device |
| Sound alert | `switch` | Enable sound detection on the device |
| Motion detected | `binary_sensor` | Turns on for ~30 s when the monitor reports motion |
| Sound detected | `binary_sensor` | Turns on for ~30 s when the monitor reports sound |
| Privacy mode | `switch` | Camera lens on/off |

Multiple monitors on one account are supported: the bridge serves each camera from the same port on its own RTSP path, derived from the camera's name.

### Local streaming

The bridge tries the local network first and falls back to the cloud whenever any
step fails, so a camera it cannot reach locally still works exactly as before.

| Stage | Transport |
|---|---|
| Address | Taken from Home Assistant, which already resolves it; UDP 6667 discovery is the fallback |
| Session | TCP 6668, protocol 3.5, authenticated with the device `localKey` |
| Signalling | protocol-302 offer/answer on the same session |
| Connectivity | ICE host candidates, checked directly rather than through a full ICE agent |
| Media | KCP over UDP, AES-128-CBC payloads, each datagram carrying an HMAC-SHA1 trailer |
| Video | H.264, repacketised into RTP for RTSP |

Once it is up, nothing in that chain touches the internet. The three credentials
it needs are fetched once at setup and cached in the config entry: `local_key`
and `uid` from the device lookup, and the P2P `password` from the RTC config -
the monitor's media login is `md5(password + "||" + local_key)`.

**The monitor must have reached the Tuya cloud at least once since it booted.**
Until it has, it ignores local signalling offers entirely: the session
negotiates, DPS queries answer, and protocol-302 frames vanish without a reply.
What it collects up there has not been captured, but the shape of the failure
says its P2P subsystem is not running rather than refusing us, since an offer
carrying a deliberately unreachable ICE server is accepted once the monitor is
online. Restoring the WAN fixes it within a minute or so with no power cycle,
and once it has connected the WAN can go away again and local streaming
continues.

A capture of a monitor booting with no route out shows what it wants: DNS for
`m2.tuyaeu.com` and `a2.tuyaeu.com` every ten to twenty seconds, and nothing
else. Once routed it pings `8.8.8.8` about once a second as a reachability
check and holds an HTTPS session to the regional API host. No NTP, so whatever
it needs, it is not the time. [PROTOCOL.md](PROTOCOL.md) has the capture.

Two details of the monitor's protocol are worth knowing, because both fail
silently:

- Its ICE server list may not be empty, and every entry must be an **IP
  literal**. Given a hostname it tries to resolve it before gathering anything,
  so with no DNS it reports no candidates at all and hangs up. The bridge sends
  a documentation address, which the monitor does send a binding request to and
  which never answers; gathering its host candidate does not depend on a reply.
- It always takes the ICE controlling role and its STUN success responses carry
  no `MESSAGE-INTEGRITY`. A conformant agent answers 487 Role Conflict or
  discards the responses, which is why the connectivity checks are done in
  `pkg/lan` instead of with pion's ICE agent.


## Supported devices

The integration talks to the Tuya API generically, so unlisted Philips Avent monitors may work. Status reflects what users have confirmed:

| Model | Status |
|---|---|
| SCD973 / SCD923 | Fully supported (primary development hardware) |
| SCD951 | Working |
| SCD643/26 | Working |
| SCD971 | Working |
| SCD921 | Partial - video is intermittent, detection inconsistent |
| SCD953/26 | Unconfirmed |

## Installation

Both halves are required: the integration talks to the monitor, the add-on serves the video.

### HACS (recommended)

1. Open HACS in your Home Assistant instance.
2. Go to **Integrations** -> **...** -> **Custom repositories**.
3. Add `https://github.com/leonardpitzu/aventproxy` as an **Integration**.
4. Search for **Philips Avent Baby Monitor** and install it.
5. Restart Home Assistant.

### Add-on

1. Go to **Settings** -> **Add-ons** -> **Add-on Store** -> **...** -> **Repositories**.
2. Add `https://github.com/leonardpitzu/aventproxy`.
3. Install **Philips Avent WebRTC Bridge**.
4. Start it.

Keep the add-on and the integration on the same version. They talk over a config file whose fields grow with each release, so an older bridge can silently miss one it needs.

### Manual

Copy `custom_components/philips_avent/` into your Home Assistant `config/custom_components/` directory and restart.

## Configuration

1. **Settings** -> **Devices & Services** -> **Add Integration** -> **Philips Avent Baby Monitor**.
2. Enter the email and password you use for the Baby Monitor+ app, and check the country matches the one the account was created in.
3. Enter the 6-digit code sent to your email.

Cameras are discovered automatically and all entities are created.

The country matters. A Philips/Tuya account lives in one regional data centre and a session from one is rejected by the others; getting it wrong shows up as a login that fails immediately after the verification code.

### Options

| Option | Default | Purpose |
|---|---|---|
| Bridge host | `localhost` | Where the bridge runs. Only change it if you host the bridge yourself, outside the add-on |
| Bridge port | `38554` | RTSP port the bridge listens on |
| Two-way audio | off | Ask the camera for talkback. Leave it off unless a client really speaks back: requesting audio makes the monitor restart a playing lullaby |

A wrong bridge host makes the camera entity flap between `Unavailable` and `Idle`, because Home Assistant marks a camera unavailable while its stream cannot be opened.

## Automations

`binary_sensor.<camera>_sound_detected` and `binary_sensor.<camera>_motion_detected` turn on for about 30 seconds when the monitor reports an event.

Detection must also be enabled on the device, via the **Motion Alert** / **Sound Alert** switches - otherwise the monitor never sends anything.

```yaml
automation:
  - alias: "Baby crying alert"
    triggers:
      - trigger: state
        entity_id: binary_sensor.baby_monitor_sound_detected
        to: "on"
    actions:
      - action: notify.mobile_app_my_phone
        data:
          title: "Sound detected in nursery"
          message: "Baby may be awake"
```

## How it works

The integration speaks the same Tuya Mobile SDK API as the official app, including the password + MFA login, so its traffic is indistinguishable from the real client.

```
+------------------------------+
|        Home Assistant        |
|                              |
|  +------------------------+  |      Tuya cloud
|  |  Integration           |<-+--->  (setup only:
|  |  entities, DPS, login  |  |       login, keys)
|  +------------------------+  |
|              | config file   |
|              v               |
|  +------------------------+  |      Monitor
|  |  Bridge add-on         |<-+--->  LAN 6667/6668
|  |  LAN first, cloud      |  |      then ICE + KCP
|  |  fallback -> RTSP      |  |      (cloud fallback:
|  |  :38554                |  |       MQTT + WebRTC)
|  +------------------------+  |
|              |               |
|              v               |
|      camera entities         |
|  rtsp://host:38554/<name>    |
+------------------------------+
```

## Camera data points

DPS are Tuya's mechanism for device control. Each data point has an ID, a code name,
a type, and read/write permissions. Values are read via `tuya.m.device.get` and
written via `tuya.m.device.dp.publish`.

The ones this integration exposes as entities:

| DPS | Code | Description | Values |
|---|---|---|---|
| 134 | `motion_switch` | Motion alert | on/off |
| 138 | `bulb_switch` | Night light | on/off |
| 139 | `decibel_switch` | Sound alert | on/off |
| 158 | `floodlight_lightness` | Brightness | 1-100 |
| 201 | `play_control` | Lullaby | play/pause/stop/next/prev |
| 207 | `sensor_temperature` | Temperature | °C x 100 |
| 209 | `play_volume` | Volume | 1-100 |
| 212 | `alarm_message` | Alarm record (motion/sound event) | base64 JSON |
| 237 | `privacy_switch` | Privacy mode | 0/1 |

### Reading DPS

```python
device = client.get_device("YOUR_DEVICE_ID")
dps = device["dps"]
temperature = dps["207"]  # raw value, divide by 100 for °C
```

### Writing DPS

```python
client.set_dps("YOUR_DEVICE_ID", {"138": True})   # night light on
client.set_dps("YOUR_DEVICE_ID", {"158": 50})      # brightness 50%
client.set_dps("YOUR_DEVICE_ID", {"201": "play"})   # play lullaby
```

### Complete DPS map

#### Video & Image

| ID | Code | Name | Type | Mode | Values/Range |
|----|------|------|------|------|-------------|
| 101 | `basic_indicator` | LED status | bool | rw | true/false |
| 102 | `ipc_flip` | Image rotation | enum | rw | `flip_none`, `flip_horizontal_mirror`, `flip_vertical_mirror`, `flip_rotate_180` |
| 237 | `privacy_switch` | Privacy mode (camera off) | enum | rw | `0` (off), `1` (on) |

#### Night Light

| ID | Code | Name | Type | Mode | Values/Range |
|----|------|------|------|------|-------------|
| 138 | `bulb_switch` | Night light on/off | bool | rw | true/false |
| 158 | `floodlight_lightness` | Brightness | value | rw | 1-100 (step 1) |
| 204 | `nightlight_color` | Color | string | rw | color string |
| 240 | `nightlight_timer` | Auto-off timer (seconds) | value | rw | 1-5400 |
| 241 | `light_timer_switch` | Timer enabled | bool | rw | true/false |
| 242 | `light_timer_display` | Timer remaining (seconds) | value | ro | -1-86400 |

#### Lullabies

| ID | Code | Name | Type | Mode | Values/Range |
|----|------|------|------|------|-------------|
| 201 | `play_control` | Playback control | enum | rw | `play`, `pause`, `stop`, `next`, `prev` |
| 202 | `play` | Play specific track | string | rw | track identifier |
| 203 | `play_mode` | Loop mode | enum | rw | `loop`, `loop1`, `shuffle` |
| 209 | `play_volume` | Volume | value | rw | 1-100 (step 1) |
| 243 | `lullaby_timer_switch` | Timer enabled | bool | rw | true/false |
| 244 | `lullaby_timer` | Auto-stop timer (seconds) | value | rw | 0-5400 |
| 245 | `lullaby_display` | Timer remaining (seconds) | value | ro | -1-86400 |
| 246 | `play_state` | Current state | enum | rw | `playing`, `stopping` |
| 248 | `play_current` | Currently playing | string | rw | JSON: `{"bizcode":"phi-no-bm","id":3542155,"errcode":0}` |
| 249 | `voice_upgrade` | Custom recording update | string | rw | - |

#### Temperature Sensor

| ID | Code | Name | Type | Mode | Values/Range |
|----|------|------|------|------|-------------|
| 207 | `sensor_temperature` | Temperature (°C x 100) | value | ro | 0-5000 (scale 2). Value 2250 = 22.50°C |
| 208 | `temp_report` | Temperature (°F x 100) | value | ro | 0-500 (scale 2) |
| 231 | `temp_max_switch` | High temp alert on | bool | rw | true/false |
| 232 | `temp_min_switch` | Low temp alert on | bool | rw | true/false |
| 233 | `temp_max_cvalue` | High temp threshold (°C x 100) | value | rw | 0-4000 (step 100) |
| 234 | `temp_min_cvalue` | Low temp threshold (°C x 100) | value | rw | 0-4000 (step 100) |
| 235 | `temp_max_fvalue` | High temp threshold (°F) | string | rw | - |
| 236 | `temp_min_fvalue` | Low temp threshold (°F) | string | rw | - |

#### Motion & Sound Detection

| ID | Code | Name | Type | Mode | Values/Range |
|----|------|------|------|------|-------------|
| 106 | `motion_sensitivity` | Motion sensitivity | enum | rw | `0` (off), `1` (low), `2` (high) |
| 134 | `motion_switch` | Motion alert on/off | bool | rw | true/false |
| 168 | `motion_area_switch` | Area detection on | bool | rw | true/false |
| 169 | `motion_area` | Detection area | string | rw | JSON: `{"num":1,"region0":{"x":0,"y":0,"xlen":100,"ylen":100}}` |
| 250 | `motion_detection` | Motion event (read-only) | string | ro | event data |
| 139 | `decibel_switch` | Sound detection on/off | bool | rw | true/false |
| 140 | `decibel_sensitivity` | Sound sensitivity | enum | rw | `0` (off), `1` (low), `2` (high) |
| 141 | `decibel_upload` | Sound event (read-only) | string | ro | `decibel_upload` when triggered |
| 239 | `monitor_sensitivity` | Background monitoring | enum | rw | `0`, `1`, `2`, `3` |

#### Two-Way Audio

| ID | Code | Name | Type | Mode | Values/Range |
|----|------|------|------|------|-------------|
| 252 | `pu_talking` | Parent unit talkback | enum | rw | `0` (off), `1` (on) |
| 253 | `app_talking` | App talkback | enum | rw | `0` (off), `1` (on) |
| 251 | `background_mode` | Background audio mode | bool | rw | true/false |

#### System

| ID | Code | Name | Type | Mode | Values/Range |
|----|------|------|------|------|-------------|
| 205 | `power_status` | Power state | enum | ro | `0` (battery), `1` (plugged) |
| 206 | `OTA_message` | Firmware update | enum | rw | `0`, `1`, `2` |
| 247 | `device_poweroff` | Power off device | enum | rw | `0`, `1` |
| 254 | `bu_reset` | Base unit reset | string | ro | - |
| 255 | `timer_report` | Report timer | enum | rw | `0`, `1` |

### Video Quality

Video quality is controlled via the WebRTC session, not DPS. The `rtc.config.get`
response includes `vedioClaritys: [2, 4, 8]`:

| Value | Quality |
|-------|---------|
| 2 | HD (1920x1080) - main stream |
| 4 | SD (640x360) - sub stream |
| 8 | Audio only |

Set the desired quality when initiating the WebRTC connection by selecting the
appropriate stream type in the SDP offer.

### Signal Strength

Not available via DPS. Can be read from the device info's network status
or via `tuya.m.device.upgrade.rssi.info.query`.

### Examples

#### Turn on night light at 30% brightness
```python
client.set_dps(cam_id, {"138": True, "158": 30})
```

#### Play lullaby, volume 40%, auto-stop after 30 minutes
```python
client.set_dps(cam_id, {
    "201": "play",
    "209": 40,
    "243": True,
    "244": 1800,
})
```

#### Stop lullaby
```python
client.set_dps(cam_id, {"201": "stop"})
```

#### Read temperature
```python
device = client.get_device(cam_id)
temp_raw = device["dps"]["207"]  # e.g. 2250
temp_c = temp_raw / 100          # 22.50 °C
```

#### Enable motion + sound alerts
```python
client.set_dps(cam_id, {
    "134": True,   # motion alert on
    "106": "2",    # high sensitivity
    "139": True,   # sound alert on
    "140": "2",    # high sensitivity
})
```

#### Enable talkback (two-way audio)
```python
client.set_dps(cam_id, {"253": "1"})  # app talking on
# audio rides the WebRTC data channel (backchannel)
```

#### Privacy mode (camera off, audio only)
```python
client.set_dps(cam_id, {"237": "1"})  # privacy on
client.set_dps(cam_id, {"237": "0"})  # privacy off
```

## Development

```bash
# Python integration
python3 -m venv .venv && source .venv/bin/activate
pip install pytest pycryptodome aiohttp voluptuous
PYTHONPATH=. pytest tests/test_philips_avent/ -v
ruff check custom_components/ --ignore E501

# Go bridge
cd avent-webrtc-bridge
go build ./... && go test ./... && gofmt -l .
./avent-webrtc-bridge direct --help
```

The signing algorithm is generic to every Tuya Thing SDK app; see [tools/apk-key-extractor/](tools/apk-key-extractor/) for extracting the keys from an arbitrary APK.

`tools/lan302_decode.py` decodes a packet capture of the LAN control channel: give it a pcap and a `localKey` and it prints the whole session decrypted.

To check the local path against a monitor without Home Assistant in the way:

```bash
avent-webrtc-bridge lan --camera-id bfc4... --ip 192.168.0.15 \
  --local-key '...' --password ... --seconds 10
```

It reports each stage as it happens and ends with the resolution and frame
count, or with the stage that failed. Add `--verbose` to trace the protocol.
`tools/fetch_local_key.py` retrieves the `localKey` if it is not already in the
config entry, and `tools/tuya_client.py` is a standalone client for poking at
the cloud API by hand.

## Protocol

Both paths end at the same RTP forwarder, so everything downstream of signalling
is shared.

### Local

| Layer | What it is |
|---|---|
| Transport | TCP 6668, Tuya protocol 3.5: an 18-byte header, AES-GCM body, 4-byte suffix. The length field covers iv, ciphertext and tag, and stops short of the suffix |
| Session key | Nonce exchange under the `localKey` (commands 3, 4, 5), then AES-GCM over the XOR of both nonces, keeping 16 bytes of ciphertext |
| Signalling | Command 32 carries a JSON offer/answer. Its GCM iv is the trace id as ASCII, not random bytes, and a random one is accepted and then ignored |
| Connectivity | ICE, host candidates only. The monitor is always controlling and its binding success responses carry no MESSAGE-INTEGRITY |
| Media | KCP over the same socket, two conversations: control, and one for video whose id varies per session. Payloads are AES-128-CBC, each datagram carrying an HMAC-SHA1 trailer keyed by the SDP `aes-key` |
| Login | `md5(password + "||" + localKey)`, in a 104-byte frame naming the user `admin` |
| Video | H.264 arriving as RTP payloads: parameter sets whole, slices already fragmented as FU-A |

### Cloud

Tuya's Mobile SDK API, signed with HMAC-SHA256 over a composite key extracted
from the app. Password plus emailed MFA yields a `sid`;
`smartlife.m.rtc.config.get` yields the ICE servers, the P2P password and the
MQTT credentials; signalling then runs over Tuya MQTT and the media is ordinary
WebRTC with DTLS-SRTP.

### Credentials

Three, all fetched once and cached in the config entry: the `localKey` and
`uid` from the device lookup, and the P2P `password` from the RTC config. None
of them expire.

For how any of this was established, including what turned out to be wrong, see
[PROTOCOL.md](PROTOCOL.md).

## Acknowledgments

The bridge is forked from [tuya-ipc-terminal](https://github.com/seydx/tuya-ipc-terminal) by seydx, which uses WebRTC and codec utilities from [go2rtc](https://github.com/AlexxIT/go2rtc) by AlexxIT. It has since diverged with Philips Avent specifics: RTP timestamp rebasing, SPS/PPS injection, RTSP backchannel audio, MQTT signalling and the local LAN path.

Upstream project: [thekoma/aventproxy](https://github.com/thekoma/aventproxy).

## License

MIT - see [LICENSE](LICENSE).

## Disclaimer

Not affiliated with, endorsed by or connected to Koninklijke Philips N.V., Tuya Inc. or any of their subsidiaries. "Philips" and "AVENT" are registered trademarks of Koninklijke Philips N.V., used here only so users can tell which device this supports. All API access uses the owner's own credentials and the same protocol as the official app.
