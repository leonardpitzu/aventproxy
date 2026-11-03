"""Tests for the bridge-config camera payload builder.

Imports the real ``build_cameras_payload`` so a contract drift (e.g. someone
changing it to omit product_id) is caught here, not just in the Go integration
tests.

``conftest.py`` registers ``philips_avent`` as a stub package, so importing
``philips_avent.payload`` never runs the integration's ``__init__.py`` and these
tests stay free of a ``homeassistant`` dependency.
"""

import json
from pathlib import Path
from typing import ClassVar

import yaml
from philips_avent.payload import (
    bridge_app_action,
    bridge_runs_as_app,
    build_bridge_config,
    build_cameras_payload,
    dps_delta,
    truncated_dps,
)

APP_CONFIG = Path(__file__).parents[2] / "aventproxy-bridge-addon" / "config.yaml"


class TestBuildCamerasPayloadShape:
    def test_product_id_propagated_for_scd951(self):
        cameras = [
            {"deviceId": "abc123", "deviceName": "Baby", "productId": "selj2idknqhjnids"},
        ]
        payload = build_cameras_payload(cameras)
        assert len(payload) == 1
        assert payload[0] == {
            "camera_id": "abc123",
            "camera_name": "Baby",
            "product_id": "selj2idknqhjnids",
            "local_key": "",
            "password": "",
            "lan_ip": "",
        }

    def test_product_id_empty_for_scd973(self):
        """No regression for devices without a productId — empty string passes through."""
        cameras = [{"deviceId": "def456", "deviceName": "Cam"}]
        assert build_cameras_payload(cameras)[0]["product_id"] == ""

    def test_mixed_cameras(self):
        cameras = [
            {"deviceId": "abc123", "deviceName": "Baby SCD951", "productId": "selj2idknqhjnids"},
            {"deviceId": "def456", "deviceName": "Baby SCD973", "productId": ""},
        ]
        payload = build_cameras_payload(cameras)
        assert payload[0]["product_id"] == "selj2idknqhjnids"
        assert payload[1]["product_id"] == ""

    def test_devid_fallback_key(self):
        """Raw Tuya discovery dicts use 'devId' not 'deviceId'."""
        cameras = [{"devId": "xyz789", "deviceName": "Cam", "productId": "someproduct"}]
        assert build_cameras_payload(cameras)[0]["camera_id"] == "xyz789"

    def test_product_key_fallback(self):
        """Some Tuya endpoints return 'productKey' instead of 'productId'."""
        cameras = [{"deviceId": "abc", "deviceName": "Cam", "productKey": "alt-product-key"}]
        assert build_cameras_payload(cameras)[0]["product_id"] == "alt-product-key"

    def test_snake_case_product_id_fallback(self):
        """A stored-entry dict (pre-conversion) uses 'product_id'."""
        cameras = [{"id": "abc", "name": "Cam", "product_id": "stored-pid"}]
        payload = build_cameras_payload(cameras)
        assert payload[0]["camera_id"] == "abc"
        assert payload[0]["camera_name"] == "Cam"
        assert payload[0]["product_id"] == "stored-pid"

    def test_name_fallback_to_default(self):
        """A camera with no name at all gets the literal default 'camera'."""
        cameras = [{"deviceId": "abc"}]
        assert build_cameras_payload(cameras)[0]["camera_name"] == "camera"

    def test_empty_input(self):
        assert build_cameras_payload([]) == []


class TestBuildCamerasPayloadPrecedence:
    """When multiple keys are present, the documented precedence applies."""

    def test_device_id_wins_over_devid(self):
        cameras = [{"deviceId": "first", "devId": "second"}]
        assert build_cameras_payload(cameras)[0]["camera_id"] == "first"

    def test_device_name_wins_over_name(self):
        cameras = [{"deviceId": "abc", "deviceName": "first", "name": "second"}]
        assert build_cameras_payload(cameras)[0]["camera_name"] == "first"

    def test_product_id_wins_over_product_key(self):
        cameras = [
            {"deviceId": "abc", "deviceName": "Cam", "productId": "id-form", "productKey": "key-form"},
        ]
        assert build_cameras_payload(cameras)[0]["product_id"] == "id-form"


class TestBuildBridgeConfig:
    """The JSON contract with ``cmd/addon/addon.go::BridgeConfig``."""

    BASE: ClassVar[dict] = {
        "signing_key": "sk",
        "sid": "az1661958",
        "ecode": "E",
        "partner": "P",
        "app_key": "AK",
        "device_id": "D",
        "package_name": "com.philips.ph.babymonitorplus",
        "api_host": "a1.tuyaus.com",
        "bridge_port": 38554,
        "cameras": [{"deviceId": "abc123", "deviceName": "Erik", "productId": "p1"}],
    }

    def test_api_host_is_written_for_the_bridge(self):
        # Without this the add-on would talk to the EU data center whatever the
        # account's region is (issues #44, #58).
        config = build_bridge_config(**self.BASE)
        assert config["api_host"] == "a1.tuyaus.com"

    def test_keys_match_the_go_struct(self):
        config = build_bridge_config(**self.BASE)
        assert set(config) == {
            "signing_key",
            "sid",
            "ecode",
            "partner",
            "app_key",
            "device_id",
            "package_name",
            "api_host",
            "uid",
            "talkback",
            "bridge_port",
            "cameras",
        }

    def test_cameras_use_the_shared_payload_builder(self):
        config = build_bridge_config(**self.BASE)
        assert config["cameras"] == [
            {
                "camera_id": "abc123",
                "camera_name": "Erik",
                "product_id": "p1",
                "local_key": "",
                "password": "",
                "lan_ip": "",
            },
        ]

    def test_config_is_json_serialisable(self):
        assert json.loads(json.dumps(build_bridge_config(**self.BASE))) == build_bridge_config(**self.BASE)


class TestAppSchema:
    """The options the integration sets against the app's ``config.yaml``.

    The Supervisor drops any option its schema does not list, and stores what is
    left. A missing key is therefore lost (a custom port silently became the
    default), and the stored options never equal what the integration sends, so
    every Home Assistant start would restart the app.
    """

    BASE: ClassVar[dict] = TestBuildBridgeConfig.BASE
    SECRETS: ClassVar[set] = {"signing_key", "sid", "ecode", "app_key"}
    CAMERA_SECRETS: ClassVar[set] = {"local_key", "password"}

    @staticmethod
    def schema() -> dict:
        return yaml.safe_load(APP_CONFIG.read_text())["schema"]

    def test_every_option_is_in_the_schema(self):
        assert set(build_bridge_config(**self.BASE)) == set(self.schema())

    def test_every_camera_field_is_in_the_schema(self):
        camera = build_bridge_config(**self.BASE)["cameras"][0]
        assert set(camera) == set(self.schema()["cameras"][0])

    def test_secrets_are_masked_in_the_configuration_tab(self):
        schema = self.schema()
        assert {key for key, kind in schema.items() if kind == "password?"} == self.SECRETS
        camera = schema["cameras"][0]
        assert {key for key, kind in camera.items() if kind == "password?"} == self.CAMERA_SECRETS

    def test_app_maps_no_host_folders(self):
        assert "map" not in yaml.safe_load(APP_CONFIG.read_text())


class TestBridgeAppAction:
    """A restart drops every stream, so it happens only when the options changed (issue #73)."""

    OPTIONS: ClassVar[dict] = build_bridge_config(**TestBuildBridgeConfig.BASE)

    def test_unchanged_options_leave_the_app_alone(self):
        assert bridge_app_action(dict(self.OPTIONS), self.OPTIONS, running=True) is None
        assert bridge_app_action(dict(self.OPTIONS), self.OPTIONS, running=False) is None

    def test_changed_options_restart_a_running_app(self):
        assert bridge_app_action({"cameras": []}, self.OPTIONS, running=True) == "restart"

    def test_changed_options_start_a_stopped_app(self):
        assert bridge_app_action({"cameras": []}, self.OPTIONS, running=False) == "start"

    def test_a_new_lan_address_counts_as_a_change(self):
        moved = json.loads(json.dumps(self.OPTIONS))
        moved["cameras"][0]["lan_ip"] = "192.168.1.15"
        assert bridge_app_action(self.OPTIONS, moved, running=True) == "restart"


class TestBridgeRunsAsApp:
    def test_default_host_is_the_app(self):
        assert bridge_runs_as_app("localhost")
        assert bridge_runs_as_app(" LocalHost ")
        assert bridge_runs_as_app("127.0.0.1")
        assert bridge_runs_as_app("")
        assert bridge_runs_as_app(None)

    def test_another_host_is_configured_by_hand(self):
        assert not bridge_runs_as_app("192.168.1.20")
        assert not bridge_runs_as_app("bridge.lan")


class TestDpsDelta:
    """The debug line that says what a poll changed (issues #42, #61)."""

    def test_only_changed_and_new_keys(self):
        old = {"207": 2310, "141": "", "212": "same"}
        new = {"207": 2600, "141": "", "212": "same", "250": "motion_detection"}
        assert dps_delta(old, new) == {"207": 2600, "250": "motion_detection"}

    def test_no_previous_state_reports_everything(self):
        assert dps_delta(None, {"207": 2310}) == {"207": 2310}
        assert dps_delta({}, {"207": 2310}) == {"207": 2310}

    def test_nothing_changed_is_empty(self):
        assert dps_delta({"207": 2310}, {"207": 2310}) == {}
        assert dps_delta({"207": 2310}, {}) == {}
        assert dps_delta({"207": 2310}, None) == {}

    def test_a_value_going_empty_still_counts(self):
        # An alert DPS clearing is a change worth seeing in the log.
        assert dps_delta({"141": "decibel_upload"}, {"141": ""}) == {"141": ""}

    def test_long_alarm_record_is_cut_but_readable(self):
        record = "e" * 900
        out = dps_delta({}, {"212": record})["212"]
        assert out.startswith("e" * 300)
        assert "+600 chars" in out
        assert len(out) < 400

    def test_short_values_are_untouched(self):
        assert dps_delta({}, {"201": "play"}) == {"201": "play"}

    def test_truncated_dps_keeps_repeated_pushes(self):
        # A push repeating a value is still an event worth logging.
        assert truncated_dps({"141": "decibel_upload"}) == {"141": "decibel_upload"}
        assert truncated_dps(None) == {}


class TestTalkbackOption:
    """Two-way audio must be opt-in (issue #72)."""

    BASE: ClassVar[dict] = {
        "signing_key": "sk",
        "sid": "eu166",
        "ecode": "E",
        "partner": "P",
        "app_key": "AK",
        "device_id": "D",
        "package_name": "pkg",
        "api_host": "a1.tuyaeu.com",
        "bridge_port": 38554,
        "cameras": [{"deviceId": "abc", "deviceName": "Erik"}],
    }

    def test_default_is_off(self):
        # Watching the camera must not claim the speaker.
        assert build_bridge_config(**self.BASE)["talkback"] is False

    def test_can_be_turned_on(self):
        assert build_bridge_config(**self.BASE, talkback=True)["talkback"] is True

    def test_key_is_part_of_the_go_contract(self):
        assert "talkback" in build_bridge_config(**self.BASE)
