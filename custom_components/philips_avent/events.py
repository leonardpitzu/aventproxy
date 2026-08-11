"""Motion and sound event payloads, per device family.

Not every monitor reports an alert the same way, which is why the binary sensors
stayed off on several models while the Philips app got the notification (issues
#40, #42, #59, #61):

- Some firmwares set DPS 250 to `motion_detection` and DPS 141 to
  `decibel_upload`.
- Others leave both empty and post the alarm to DPS 212 instead, as base64 JSON
  describing the snapshot it uploaded:
  `{"v":"4.0","cmd":"ipc_motion","alarm":true,"time":1783686591,"files":[...]}`
  (from the diagnostics on #61). A noise alert uses the same shape with
  `cmd: ipc_bang` (from the diagnostics on #42).

Which of the two applies is not a property of the model. DPS 212 was taken for
an SCD951-and-SCD953 trait until an SCD973/26 was watched through a run of
alarms and reported every one of them in 212, with 250 and 141 empty the whole
time (2026-08-11). So both paths are read on every monitor.

DPS 212 carries its own timestamp, which makes it usable on the cloud poll as
well: the value sticks around in device state, so freshness comes from the
timestamp rather than from having seen the push arrive.

One alarm at a time, whichever key carries it: the monitor detects motion or
sound, never both, and DPS 134 and 139 select which. A test that turns both on
measures only the one set last.

No Home Assistant imports here, so the parsing is unit-tested on its own.
"""

from __future__ import annotations

import base64
import binascii
import json
import logging

_LOGGER = logging.getLogger(__name__)

# An event older than this is history, not something to turn a sensor on for.
# DPS 212 keeps the last alarm indefinitely, so without a cap a restart would
# replay whatever it finds. The cap has to stay above the slowest cloud poll
# (POLL_SLOW, 120s, used whenever the LAN client is connected) or a real alarm
# that lands between two polls is discarded as history, which is half of what
# kept the sensors quiet on #42. Replay protection does not rest on this number:
# `is_new_event` also requires a stamp newer than the last one acted on.
EVENT_MAX_AGE_SECONDS = 180.0

MOTION_COMMANDS = frozenset({"ipc_motion", "ipc_move", "motion"})
# `ipc_bang` is what the SCD953 posts for a noise alert, confirmed by the
# diagnostics on #42. Until it was listed here the sound sensor stayed off while
# the Philips app notified. A cry is a sound alert on a baby monitor, so
# `ipc_cry` belongs here too.
SOUND_COMMANDS = frozenset({"ipc_bang", "ipc_cry", "ipc_sound", "ipc_decibel", "sound", "decibel"})

KNOWN_COMMANDS = MOTION_COMMANDS | SOUND_COMMANDS

# Alarm commands already reported, so an unmapped one is logged once per run
# instead of on every poll.
_seen_unknown_commands: set[str] = set()


def decode_event_payload(raw: object) -> dict | None:
    """Decode a DPS value that carries an event as base64-encoded JSON.

    Accepts plain JSON too, since not every firmware base64s it. Returns None for
    anything unreadable: an event we cannot parse must not turn a sensor on.
    """
    if isinstance(raw, dict):
        return raw
    if not isinstance(raw, str) or not raw.strip():
        return None

    text = raw.strip()
    if not text.startswith("{"):
        try:
            text = base64.b64decode(text, validate=True).decode("utf-8")
        except (binascii.Error, ValueError, UnicodeDecodeError):
            return None

    try:
        payload = json.loads(text)
    except (json.JSONDecodeError, ValueError):
        return None
    return payload if isinstance(payload, dict) else None


def event_timestamp(payload: dict | None) -> float | None:
    """Seconds-since-epoch stamp of an event payload, if it has one."""
    if not payload:
        return None
    value = payload.get("time")
    if isinstance(value, bool) or not isinstance(value, (int, float, str)):
        return None
    try:
        stamp = float(value)
    except (TypeError, ValueError):
        return None
    # Some firmwares report milliseconds.
    if stamp > 1e11:
        stamp /= 1000.0
    return stamp if stamp > 0 else None


def alarm_event_timestamp(raw: object, commands: frozenset[str]) -> float | None:
    """Timestamp of an alarm of the given kind carried in a DPS value.

    Returns None when the payload is unreadable, is not one of `commands`, or
    says `alarm: false`.
    """
    payload = decode_event_payload(raw)
    if not payload:
        return None
    command = str(payload.get("cmd", "")).lower()
    if command not in commands:
        note_unmapped_command(command)
        return None
    if payload.get("alarm") is False:
        return None
    return event_timestamp(payload)


def note_unmapped_command(command: str) -> None:
    """Log an alarm command no sensor maps to, once per command per run.

    Every family found so far names its alerts differently, and an unmapped one
    used to vanish without trace: the monitor raised an alarm, the Philips app
    notified, Home Assistant showed nothing and the log said nothing either
    (issues #42, #61). Naming it turns the next family into a one-line change
    instead of decoding a diagnostics dump by hand.
    """
    if not command or command in KNOWN_COMMANDS or command in _seen_unknown_commands:
        return
    _seen_unknown_commands.add(command)
    _LOGGER.warning(
        "Monitor reported alarm command '%s', which no motion or sound sensor maps to yet. "
        "Please report it at https://github.com/leonardpitzu/aventproxy/issues so it can be added",
        command,
    )


def motion_event_timestamp(raw: object) -> float | None:
    """Timestamp of a motion alarm carried in a DPS value (SCD951 DPS 212)."""
    return alarm_event_timestamp(raw, MOTION_COMMANDS)


def sound_event_timestamp(raw: object) -> float | None:
    """Timestamp of a sound alarm carried in a DPS value."""
    return alarm_event_timestamp(raw, SOUND_COMMANDS)


# A LAN session only carries alarms once it negotiates a session key, which is
# protocol 3.4 and up. On 3.3 the alarm keys never arrive and the cloud poll is
# the only way one is ever seen. Measured both ways: an SCD951 owner on 3.3 saw
# nothing, and on 3.5 saw a motion record pushed with the sensor firing 1.3
# seconds later against 35 through the poll (#61, rc9); on an SCD973/26 at 3.5,
# three motion alarms in a row each pushed within 2.4 seconds and none needed
# the poll (2026-08-11).
LAN_ALARM_PUSH_MIN_VERSION = 3.4


def alarms_push_over_lan(protocol_version: float | None) -> bool:
    """Whether a session at this protocol delivers alarm records as pushes."""
    return protocol_version is not None and protocol_version >= LAN_ALARM_PUSH_MIN_VERSION


def alert_selection(turn_on: str, turns_off: str | None) -> dict[str, bool]:
    """The write that leaves the monitor detecting exactly what was asked for.

    The monitor detects motion or sound, never both, but it does not keep its
    two switches consistent: setting one leaves the other reading True and only
    the last one written is acted on (measured 2026-08-11 - the monitor pushed
    back the key that was set and nothing else). Turning one on therefore has to
    turn the other off, or Home Assistant shows two detectors running while one
    is silently doing nothing.

    Turning one off is not paired: it disables detection, and what the other
    switch says still stands.
    """
    if turns_off is None:
        return {turn_on: True}
    return {turn_on: True, turns_off: False}


def cloud_poll_needed(lan_connected: bool, has_alarm_record: bool, protocol_version: float | None) -> bool:
    """Whether the cloud has anything left to tell us about this monitor.

    A LAN session delivers state changes as they happen, so while one is up the
    cloud is not a second opinion, it is the same state fetched over the
    internet.

    The one thing that can be missing from it is an alarm. Monitors reporting
    alarms in DPS 212 need a session that can push it, and that turns on the
    protocol version rather than on the model: the same key that never appeared
    on 3.3 arrives within a second or two on 3.5.
    """
    if not lan_connected:
        return True
    if not has_alarm_record:
        return False
    return not alarms_push_over_lan(protocol_version)


def is_new_event(
    timestamp: float | None,
    last_seen: float | None,
    now: float,
    max_age: float = EVENT_MAX_AGE_SECONDS,
) -> bool:
    """Whether a timestamped event should fire a sensor now.

    True only when the event is newer than the last one acted on and recent
    enough to be happening rather than remembered. A stamp from the future is
    accepted: a monitor whose clock runs ahead still reports real alarms.
    """
    if timestamp is None:
        return False
    if last_seen is not None and timestamp <= last_seen:
        return False
    return timestamp >= now - max_age


# A lullaby state that flips and flips back inside this window is not a state
# change, it is the camera re-announcing itself when a stream session ends. Real
# transitions observed on hardware stand alone; the spurious pairs measured on
# 2026-07-31 were 270 to 290 ms apart (issue #72). Two seconds is comfortably
# above that and still fast enough that a lullaby started from Home Assistant
# looks immediate.
LULLABY_SETTLE_SECONDS = 2.0


def lullaby_state_settled(
    pending: str | None,
    pending_since: float | None,
    now: float,
    settle: float = LULLABY_SETTLE_SECONDS,
) -> bool:
    """Whether a held lullaby state has stood still long enough to be believed.

    The camera emits `stopping` immediately followed by `playing` at the end of
    every viewing session, which reached the binary sensor as a flicker and could
    leave it disagreeing with the room. Holding a value briefly means such a pair
    cancels itself and never becomes entity state.
    """
    if pending is None or pending_since is None:
        return False
    return now - pending_since >= settle
