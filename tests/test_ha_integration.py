"""Integration behaviour that needs a running Home Assistant (pytest-homeassistant).

The tests under test_philips_avent/ cover the Home Assistant free modules through
a stub package; these cover setup, the flows, the select and diagnostics through
custom_components.philips_avent itself.
"""

from unittest.mock import AsyncMock, MagicMock, patch

from homeassistant.config_entries import ConfigEntryState
from homeassistant.const import CONF_COUNTRY, CONF_EMAIL, CONF_PASSWORD
from homeassistant.core import HomeAssistant
from homeassistant.data_entry_flow import FlowResultType
from pytest_homeassistant.common import MockConfigEntry

from custom_components.philips_avent.api import TuyaAPIError
from custom_components.philips_avent.config_flow import PhilipsAventConfigFlow
from custom_components.philips_avent.const import (
    CONF_BRIDGE_HOST,
    CONF_BRIDGE_PORT,
    CONF_SID,
    CONF_TALKBACK,
    CONF_UID,
    DOMAIN,
    DPS_LIGHT_TIMER,
    DPS_LIGHT_TIMER_SWITCH,
)
from custom_components.philips_avent.diagnostics import async_get_config_entry_diagnostics
from custom_components.philips_avent.select import AventTimerSelect

UID = "eu0000000000demo"
CAMERA = {
    "id": "cam1",
    "name": "Nursery",
    "product_id": "selj2idknqhjnids",
    "local_key": "local-key",
    "password": "lan-password",
    "lan_ip": "",
}
PACKAGE = "custom_components.philips_avent"


def _entry(hass: HomeAssistant, cameras: list | None = None) -> MockConfigEntry:
    entry = MockConfigEntry(
        domain=DOMAIN,
        unique_id=UID,
        data={
            CONF_EMAIL: "rex@example.invalid",
            CONF_SID: "sid",
            CONF_UID: UID,
            CONF_COUNTRY: "NL",
            "device_id": "dev1",
            "cameras": [CAMERA] if cameras is None else cameras,
        },
    )
    entry.add_to_hass(hass)
    return entry


def _api(discovered=None, error: Exception | None = None) -> MagicMock:
    api = MagicMock()
    api.discover_cameras = AsyncMock(return_value=discovered or [], side_effect=error)
    return api


# ---------------------------------------------------------------------------
# setup
# ---------------------------------------------------------------------------


async def test_no_cameras_is_a_setup_error_not_a_bool(hass: HomeAssistant) -> None:
    entry = _entry(hass, cameras=[])
    with patch(f"{PACKAGE}.PhilipsAventAPI", return_value=_api()):
        await hass.config_entries.async_setup(entry.entry_id)
    assert entry.state is ConfigEntryState.SETUP_ERROR


async def test_a_transient_discovery_failure_is_retried(hass: HomeAssistant) -> None:
    entry = _entry(hass, cameras=[])
    with patch(f"{PACKAGE}.PhilipsAventAPI", return_value=_api(error=TuyaAPIError("API_BUSY", "try later"))):
        await hass.config_entries.async_setup(entry.entry_id)
    assert entry.state is ConfigEntryState.SETUP_RETRY


async def test_an_expired_session_during_discovery_asks_to_reauthenticate(hass: HomeAssistant) -> None:
    entry = _entry(hass, cameras=[])
    with patch(f"{PACKAGE}.PhilipsAventAPI", return_value=_api(error=TuyaAPIError("USER_SESSION_LOSS", "gone"))):
        await hass.config_entries.async_setup(entry.entry_id)
    assert entry.state is ConfigEntryState.SETUP_ERROR
    assert [flow["context"]["source"] for flow in hass.config_entries.flow.async_progress()] == ["reauth"]


async def test_learning_a_lan_address_does_not_reload_the_entry(hass: HomeAssistant) -> None:
    """The address write-back used to trip the update listener and restart every camera."""
    entry = _entry(hass)
    coordinator = MagicMock(stop_lan=AsyncMock())
    with (
        patch(f"{PACKAGE}.PhilipsAventAPI", return_value=_api()),
        patch(f"{PACKAGE}._setup_camera", AsyncMock(return_value=("cam1", coordinator))),
        patch(f"{PACKAGE}._configure_bridge", AsyncMock()),
        patch(f"{PACKAGE}.async_call_later", MagicMock()),
        patch.object(hass.config_entries, "async_forward_entry_setups", AsyncMock()),
    ):
        assert await hass.config_entries.async_setup(entry.entry_id)
    assert entry.state is ConfigEntryState.LOADED
    assert not entry.update_listeners

    with patch.object(hass.config_entries, "async_reload", AsyncMock()) as reload:
        hass.config_entries.async_update_entry(
            entry, data={**entry.data, "cameras": [{**CAMERA, "lan_ip": "192.0.2.5"}]}
        )
        await hass.async_block_till_done()
    reload.assert_not_called()


# ---------------------------------------------------------------------------
# flows
# ---------------------------------------------------------------------------


async def _reauth_with_uid(hass: HomeAssistant, entry: MockConfigEntry, uid: str):
    flow = await entry.start_reauth_flow(hass)
    assert flow["step_id"] == "reauth_confirm"
    with (
        patch.object(PhilipsAventConfigFlow, "_async_begin_login", AsyncMock()),
        patch.object(
            PhilipsAventConfigFlow, "_async_complete_login", AsyncMock(return_value={"sid": "new-sid", "uid": uid})
        ),
        patch.object(PhilipsAventConfigFlow, "_region_data", return_value={}),
        patch(f"{PACKAGE}.async_setup_entry", AsyncMock(return_value=True)),
    ):
        result = await hass.config_entries.flow.async_configure(
            flow["flow_id"], {CONF_EMAIL: "rex@example.invalid", CONF_PASSWORD: "pw", CONF_COUNTRY: "NL"}
        )
        assert result["step_id"] == "reauth_mfa"
        result = await hass.config_entries.flow.async_configure(result["flow_id"], {"mfa_code": "123456"})
        await hass.async_block_till_done()
    return result


async def test_reauth_with_another_account_is_refused(hass: HomeAssistant) -> None:
    entry = _entry(hass)
    result = await _reauth_with_uid(hass, entry, "eu9999999999else")
    assert result["type"] is FlowResultType.ABORT
    assert result["reason"] == "unique_id_mismatch"
    assert entry.data[CONF_SID] == "sid"


async def test_reauth_with_the_same_account_updates_the_session(hass: HomeAssistant) -> None:
    entry = _entry(hass)
    result = await _reauth_with_uid(hass, entry, UID)
    assert result["type"] is FlowResultType.ABORT
    assert result["reason"] == "reauth_successful"
    assert entry.data[CONF_SID] == "new-sid"
    assert entry.data["cameras"] == [CAMERA]


async def test_changing_options_reloads_the_entry(hass: HomeAssistant) -> None:
    entry = _entry(hass)
    with (
        patch(f"{PACKAGE}.async_setup_entry", AsyncMock(return_value=True)) as setup,
        patch(f"{PACKAGE}.async_unload_entry", AsyncMock(return_value=True)),
    ):
        assert await hass.config_entries.async_setup(entry.entry_id)
        flow = await hass.config_entries.options.async_init(entry.entry_id)
        await hass.config_entries.options.async_configure(
            flow["flow_id"], {CONF_BRIDGE_HOST: "127.0.0.1", CONF_BRIDGE_PORT: 8554, CONF_TALKBACK: True}
        )
        await hass.async_block_till_done()
    assert setup.await_count == 2


# ---------------------------------------------------------------------------
# entities and diagnostics
# ---------------------------------------------------------------------------


def _timer(data: dict) -> AventTimerSelect:
    coordinator = MagicMock(data=data, device_info={})
    return AventTimerSelect(coordinator, "cam1", "Light timer", "mdi:timer", DPS_LIGHT_TIMER, DPS_LIGHT_TIMER_SWITCH)


def test_timer_select_reports_unknown_for_missing_data_points() -> None:
    assert _timer({"101": True}).current_option is None
    assert _timer({DPS_LIGHT_TIMER_SWITCH: True}).current_option is None


def test_timer_select_reads_reported_data_points() -> None:
    assert _timer({DPS_LIGHT_TIMER_SWITCH: False}).current_option == "Off"
    assert _timer({DPS_LIGHT_TIMER_SWITCH: True, DPS_LIGHT_TIMER: 600}).current_option == "10 min"


async def test_diagnostics_redact_addresses_and_location(hass: HomeAssistant) -> None:
    entry = _entry(hass, cameras=[{**CAMERA, "lan_ip": "192.0.2.5"}])
    coordinator = MagicMock(
        camera_name="Nursery",
        data={"101": True},
        lan_connected=True,
        update_interval=None,
        device_info={"ip": "198.51.100.7", "lat": "52.0", "lon": "4.0", "mac": "aa:bb", "productId": "selj"},
    )
    entry.runtime_data = MagicMock(coordinators={"cam1": coordinator})

    diag = await async_get_config_entry_diagnostics(hass, entry)

    camera = diag["config_entry"]["cameras"][0]
    assert camera["lan_ip"] == camera["local_key"] == camera["password"] == "**REDACTED**"
    device = diag["devices"]["cam1"]["device_info"]
    assert {device[key] for key in ("ip", "lat", "lon", "mac")} == {"**REDACTED**"}
    assert device["productId"] == "selj"
