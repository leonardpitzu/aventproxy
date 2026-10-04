"""Tests for SenseIQ payloads (issue #74).

Every DPS 4 sample is verbatim from an SCD973/26 LAN session on 2026-10-04,
taken in the first twenty minutes after SenseIQ was switched on with a baby in
bed. The schema excerpts are from the same monitor's cloud device record.
"""

import base64
import json

from philips_avent.senseiq import (
    BREATHING_FRESH_SECONDS,
    STAGE_AWAKE,
    STAGE_LIGHT,
    STAGE_OUT,
    STAGE_SETTLING,
    BreathingReading,
    has_senseiq,
    is_fresh,
    occupied_reading,
    parse_breathing,
    parse_sleep_session,
)

# 21:42:16, a fresh session: {"st":1791139245,"sd":0,"css":"i","cssd":0,"ssd":[]}
SESSION_START = (
    "N2IyMjczNzQyMjNhMzEzNzM5MzEzMTMzMzkzMjM0MzUyYzIyNzM2NDIyM2EzMDJjMjI2MzczNzMy"
    "MjNhMjI2OTIyMmMyMjYzNzM3MzY0MjIzYTMwMmMyMjczNzM2NDIyM2E1YjVkN2Q="
)
# 21:44:48, the settling stretch over and filed as "o":
# {"st":1791139245,"sd":243,"css":"l","cssd":0,"ssd":[{"o":243}]}
SESSION_FIRST_LIGHT = (
    "N2IyMjczNzQyMjNhMzEzNzM5MzEzMTMzMzkzMjM0MzUyYzIyNzM2NDIyM2EzMjM0MzMyYzIyNjM3"
    "MzczMjIzYTIyNmMyMjJjMjI2MzczNzM2NDIyM2EzMDJjMjI3MzczNjQyMjNhNWI3YjIyNmYyMjNh"
    "MzIzNDMzN2Q1ZDdk"
)
# 22:00:27: {"st":1791139245,"sd":1182,"css":"l","cssd":601,
#            "ssd":[{"o":243},{"l":231},{"a":107}]}
SESSION_LATER = (
    "N2IyMjczNzQyMjNhMzEzNzM5MzEzMTMzMzkzMjM0MzUyYzIyNzM2NDIyM2EzMTMxMzgzMjJjMjI2"
    "MzczNzMyMjNhMjI2YzIyMmMyMjYzNzM3MzY0MjIzYTM2MzAzMTJjMjI3MzczNjQyMjNhNWI3YjIy"
    "NmYyMjNhMzIzNDMzN2QyYzdiMjI2YzIyM2EzMjMzMzE3ZDJjN2IyMjYxMjIzYTMxMzAzNzdkNWQ3"
    "ZA=="
)


def _encode(session: dict) -> str:
    return base64.b64encode(json.dumps(session).encode().hex().encode()).decode()


class TestSchema:
    def test_scd973_schema_has_senseiq(self):
        schema = json.dumps(
            [
                {"code": "sleepiq_switch", "id": 1},
                {"code": "sleepiq_status", "id": 3},
                {"code": "sleep_session_data", "id": 4},
            ]
        )
        assert has_senseiq(schema)

    def test_schema_without_senseiq(self):
        schema = json.dumps([{"code": "motion_switch", "id": 134}, {"code": "sensor_temperature", "id": 207}])
        assert not has_senseiq(schema)

    def test_missing_or_broken_schema(self):
        assert not has_senseiq(None)
        assert not has_senseiq("")
        assert not has_senseiq("not json")
        assert not has_senseiq("{}")


class TestBreathing:
    def test_still_reading_has_a_rate(self):
        reading = parse_breathing('{"r":"b","br":21}')
        assert reading == BreathingReading("b", 21)
        assert reading.in_bed
        assert not reading.moving
        assert reading.breathing_rate == 21

    def test_moving_reading_has_no_rate(self):
        reading = parse_breathing('{"r":"m","br":0}')
        assert reading.in_bed
        assert reading.moving
        assert reading.breathing_rate is None

    def test_out_of_bed_reading(self):
        reading = parse_breathing('{"r":"o","br":0}')
        assert not reading.in_bed
        assert reading.breathing_rate is None

    def test_cloud_copy_is_not_a_reading(self):
        # The cloud device record carries DPS 3 as an empty string.
        assert parse_breathing("") is None
        assert parse_breathing(None) is None
        assert parse_breathing("{}") is None
        assert parse_breathing('{"br":20}') is None

    def test_bad_rate_keeps_the_reading(self):
        assert parse_breathing('{"r":"b","br":"x"}') == BreathingReading("b", None)


class TestFreshness:
    def test_reading_goes_stale(self):
        assert is_fresh(100.0, 100.0 + BREATHING_FRESH_SECONDS - 1)
        assert not is_fresh(100.0, 100.0 + BREATHING_FRESH_SECONDS)
        assert not is_fresh(None, 100.0)

    def test_occupied_only_while_fresh_and_in_bed(self):
        still = BreathingReading("b", 20)
        assert occupied_reading(still, 100.0, 110.0) is still
        assert occupied_reading(still, 100.0, 100.0 + BREATHING_FRESH_SECONDS) is None
        assert occupied_reading(BreathingReading("o", 0), 100.0, 110.0) is None
        assert occupied_reading(None, None, 110.0) is None


class TestSleepSession:
    def test_session_starts_settling(self):
        session = parse_sleep_session(SESSION_START)
        assert session.started == 1791139245
        assert session.duration == 0
        assert session.stage == STAGE_SETTLING
        assert session.history == ()
        assert session.asleep == 0

    def test_settling_is_filed_as_out(self):
        session = parse_sleep_session(SESSION_FIRST_LIGHT)
        assert session.stage == STAGE_LIGHT
        assert session.history == ((STAGE_OUT, 243),)
        assert session.asleep == 0

    def test_sleep_time_counts_light_and_deep_only(self):
        session = parse_sleep_session(SESSION_LATER)
        assert session.duration == 1182
        assert session.stage == STAGE_LIGHT
        assert session.stage_duration == 601
        assert session.history == ((STAGE_OUT, 243), (STAGE_LIGHT, 231), (STAGE_AWAKE, 107))
        assert session.asleep == 231 + 601
        assert session.awake == 107

    def test_deep_sleep_counts(self):
        session = parse_sleep_session(
            _encode({"st": 1, "sd": 600, "css": "d", "cssd": 200, "ssd": [{"d": 100}, {"a": 50}, {"l": 250}]})
        )
        assert session.asleep == 100 + 250 + 200

    def test_plain_json_accepted(self):
        session = parse_sleep_session('{"st":5,"sd":10,"css":"a","cssd":10,"ssd":[]}')
        assert session.stage == STAGE_AWAKE

    def test_unknown_stage_is_none(self):
        session = parse_sleep_session(_encode({"st": 1, "sd": 1, "css": "z", "cssd": 1, "ssd": [{"z": 5}]}))
        assert session.stage is None
        assert session.history == ((None, 5),)

    def test_unreadable_values(self):
        assert parse_sleep_session(None) is None
        assert parse_sleep_session("") is None
        assert parse_sleep_session("not base64!") is None
        assert parse_sleep_session(base64.b64encode(b"zz").decode()) is None

    def test_malformed_history_entries_are_skipped(self):
        session = parse_sleep_session(
            _encode({"st": 1, "sd": 9, "css": "l", "cssd": 1, "ssd": [{"l": 4}, {"a": 1, "l": 2}, {"d": "x"}, 7]})
        )
        assert session.history == ((STAGE_LIGHT, 4),)
