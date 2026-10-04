"""SenseIQ: the breathing and sleep data a monitor computes for itself (issue #74).

Only the newer generation has it, the SCD923 and the SCD973. Its device schema
names the two data points, and the older SCD951 and SCD953 lack both, so the
schema decides whether a monitor gets these entities at all.

- DPS 3, `sleepiq_status`, every one to ten seconds while a baby is in the
  SenseIQ zone: ``{"r": "b", "br": 21}``. ``r`` is ``b`` (still, breathing rate
  measured), ``m`` (moving, ``br`` is 0) or ``o`` (out of bed, seen once on a
  fresh connection). When the crib empties the pushes simply stop, so silence
  is the out-of-bed signal.
- DPS 4, `sleep_session_data`, about once a minute: JSON, hex encoded, then
  base64. ``st`` is when the session started (epoch seconds), ``sd`` its length
  in seconds, ``css`` the current stage and ``cssd`` how long it has lasted,
  ``ssd`` the finished stages in order as single-key objects (``{"l": 231}``).

Stage codes: ``a`` awake, ``l`` light, ``d`` deep, ``o`` out of bed, and ``i``,
which an SCD973 showed for the first three to four minutes of a session before
filing that stretch in ``ssd`` as ``o``. It reads as the monitor settling on a
reading rather than anything about the baby. ``sd`` keeps counting while the
baby is out, so it is a session length, not a sleep time.

No Home Assistant imports here, so the unit tests can load it directly.
"""

from __future__ import annotations

import base64
import binascii
import json
from dataclasses import dataclass
from typing import Any

# Readings stop when the baby leaves the zone, so one older than this means
# nobody is in bed. An SCD973 pushed at most about 15 s apart while occupied.
BREATHING_FRESH_SECONDS = 60

READING_STILL = "b"
READING_MOVING = "m"

STAGE_AWAKE = "awake"
STAGE_LIGHT = "light"
STAGE_DEEP = "deep"
STAGE_OUT = "out"
STAGE_SETTLING = "settling"
SLEEP_STAGES = {
    "a": STAGE_AWAKE,
    "l": STAGE_LIGHT,
    "d": STAGE_DEEP,
    "o": STAGE_OUT,
    "i": STAGE_SETTLING,
}
ASLEEP_STAGES = (STAGE_LIGHT, STAGE_DEEP)

SCHEMA_CODES = ("sleepiq_status", "sleep_session_data")


def has_senseiq(schema: Any) -> bool:
    """Whether a device schema declares the SenseIQ data points.

    The cloud returns the schema as a JSON string of data point definitions.
    """
    if isinstance(schema, str):
        try:
            schema = json.loads(schema)
        except ValueError:
            return False
    if not isinstance(schema, list):
        return False
    return any(isinstance(dp, dict) and dp.get("code") in SCHEMA_CODES for dp in schema)


@dataclass(frozen=True)
class BreathingReading:
    """One DPS 3 push."""

    reading: str
    rate: int | None

    @property
    def in_bed(self) -> bool:
        return self.reading in (READING_STILL, READING_MOVING)

    @property
    def moving(self) -> bool:
        return self.reading == READING_MOVING

    @property
    def breathing_rate(self) -> int | None:
        """Breaths per minute, only measured while the baby lies still."""
        if self.reading == READING_STILL and self.rate:
            return self.rate
        return None


def _as_dict(raw: Any) -> dict | None:
    if isinstance(raw, dict):
        return raw
    if not isinstance(raw, str) or not raw:
        return None
    try:
        value = json.loads(raw)
    except ValueError:
        return None
    return value if isinstance(value, dict) else None


def parse_breathing(raw: Any) -> BreathingReading | None:
    """Parse a DPS 3 value, or None when it is not a reading.

    The cloud copy of DPS 3 is an empty string, which is not a reading.
    """
    data = _as_dict(raw)
    if not data or not isinstance(data.get("r"), str):
        return None
    try:
        rate = int(data["br"]) if data.get("br") is not None else None
    except (TypeError, ValueError):
        rate = None
    return BreathingReading(data["r"], rate)


def is_fresh(received_at: float | None, now: float) -> bool:
    return received_at is not None and now - received_at < BREATHING_FRESH_SECONDS


def occupied_reading(
    reading: BreathingReading | None, received_at: float | None, now: float
) -> BreathingReading | None:
    """The reading while a baby is in bed, or None once the crib is empty."""
    if reading is None or not reading.in_bed or not is_fresh(received_at, now):
        return None
    return reading


def _decode_session(raw: Any) -> dict | None:
    """DPS 4 is base64 of hex of JSON; plain JSON is accepted as well."""
    data = _as_dict(raw)
    if data is not None:
        return data
    if not isinstance(raw, str) or not raw:
        return None
    try:
        text = base64.b64decode(raw, validate=True).decode("ascii")
        return _as_dict(bytes.fromhex(text).decode("utf-8"))
    except (binascii.Error, ValueError, UnicodeDecodeError):
        return None


def _seconds(value: Any) -> int | None:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    return int(value)


def stage_name(code: Any) -> str | None:
    """A stage code as a name, or None for a code not seen yet."""
    return SLEEP_STAGES.get(code) if isinstance(code, str) else None


@dataclass(frozen=True)
class SleepSession:
    """One DPS 4 push."""

    started: int | None
    duration: int | None
    stage: str | None
    stage_duration: int | None
    history: tuple[tuple[str | None, int], ...]

    def _total(self, stages: tuple[str, ...]) -> int:
        finished = sum(seconds for stage, seconds in self.history if stage in stages)
        current = (self.stage_duration or 0) if self.stage in stages else 0
        return finished + current

    @property
    def asleep(self) -> int:
        """Seconds of light and deep sleep, the current stage included."""
        return self._total(ASLEEP_STAGES)

    @property
    def awake(self) -> int:
        return self._total((STAGE_AWAKE,))


def parse_sleep_session(raw: Any) -> SleepSession | None:
    """Parse a DPS 4 value, or None when it cannot be read."""
    data = _decode_session(raw)
    if not data:
        return None
    history = []
    for item in data.get("ssd") or []:
        if isinstance(item, dict) and len(item) == 1:
            code, seconds = next(iter(item.items()))
            seconds = _seconds(seconds)
            if seconds is not None:
                history.append((stage_name(code), seconds))
    return SleepSession(
        started=_seconds(data.get("st")),
        duration=_seconds(data.get("sd")),
        stage=stage_name(data.get("css")),
        stage_duration=_seconds(data.get("cssd")),
        history=tuple(history),
    )
