"""Philips Avent Baby Monitor integration for Home Assistant."""

from __future__ import annotations

import asyncio
import logging

from homeassistant.config_entries import ConfigEntry
from homeassistant.const import CONF_PASSWORD, Platform
from homeassistant.core import HomeAssistant
from homeassistant.helpers.aiohttp_client import async_get_clientsession
from homeassistant.helpers.event import async_call_later

from .api import PhilipsAventAPI
from .bridge_app import async_configure_bridge_app, async_reset_bridge_app
from .const import (
    CONF_API_HOST,
    CONF_BRIDGE_HOST,
    CONF_BRIDGE_PORT,
    CONF_COUNTRY_CODE,
    CONF_DEVICE_ID,
    CONF_ECODE,
    CONF_PARTNER,
    CONF_SID,
    CONF_TALKBACK,
    CONF_UID,
    DEFAULT_BRIDGE_HOST,
    DEFAULT_BRIDGE_PORT,
    DEFAULT_TALKBACK,
    TUYA_APP_KEY,
    TUYA_DEFAULT_COUNTRY_CODE,
    TUYA_PACKAGE_NAME,
    TUYA_SIGNING_KEY,
)
from .coordinator import PhilipsAventConfigEntry, PhilipsAventCoordinator, PhilipsAventData
from .payload import bridge_runs_as_app, build_bridge_config
from .region import DEFAULT_DATA_CENTER, api_host, api_url_for_host

_LOGGER = logging.getLogger(__name__)

PLATFORMS = [
    Platform.BINARY_SENSOR,
    Platform.BUTTON,
    Platform.CAMERA,
    Platform.NUMBER,
    Platform.SELECT,
    Platform.SENSOR,
    Platform.SWITCH,
]

# tinytuya's scan retries the broadcast several times before it resolves an
# address, so the bridge cannot be told about it during setup.
LAN_ADDRESS_SETTLE = 60


def _entry_api_host(entry: ConfigEntry) -> str:
    """API host for this account, falling back to Central Europe.

    Entries created before data-center routing existed have no host stored; EU
    is the right fallback for them because that was the only host the
    integration ever used (issues #44, #58).
    """
    return entry.data.get(CONF_API_HOST) or api_host(DEFAULT_DATA_CENTER)


def _persist_lan_secrets(hass: HomeAssistant, entry: ConfigEntry, cameras: list) -> None:
    """Keep the LAN credentials on the entry so a restart needs no cloud call.

    Only writes when something actually changed: rewriting the entry on every
    start would also change the bridge app's options and restart it (issue #73).
    """
    stored = entry.data.get("cameras", [])
    if not stored:
        return
    fresh = {cam.get("deviceId") or cam.get("devId"): cam for cam in cameras}
    updated, changed = [], False
    for cam in stored:
        merged = dict(cam)
        found = fresh.get(cam.get("id"), {})
        for stored_key, api_key in (
            ("local_key", "localKey"),
            ("password", "password"),
            ("lan_ip", "lanIp"),
        ):
            value = found.get(api_key) or ""
            if value and cam.get(stored_key) != value:
                merged[stored_key] = value
                changed = True
        updated.append(merged)
    if changed:
        hass.config_entries.async_update_entry(entry, data={**entry.data, "cameras": updated})
        _LOGGER.info("Stored LAN credentials for %d camera(s)", len(updated))


async def _lan_password(api: PhilipsAventAPI, cam_id: str) -> str:
    """The monitor's P2P login secret.

    Only the RTC config carries it; the device object has no password field at
    all, so this is a second call and not a spare copy of the first.
    """
    try:
        config = await api.get_rtc_config(cam_id)
    except Exception as err:  # noqa: BLE001 - no RTC config just means no LAN path
        _LOGGER.debug("No RTC config for %s, LAN streaming stays off: %s", cam_id, err)
        return ""
    return config.get("password") or ""


async def _configure_bridge(hass: HomeAssistant, entry: ConfigEntry, api: PhilipsAventAPI, cameras: list) -> None:
    """Hand the bridge app this entry's session and camera credentials.

    A bridge on another host is configured by whoever runs it, so only the app
    next to Home Assistant is touched.
    """
    if not bridge_runs_as_app(entry.options.get(CONF_BRIDGE_HOST, DEFAULT_BRIDGE_HOST)):
        return
    options = build_bridge_config(
        signing_key=TUYA_SIGNING_KEY,
        sid=entry.data[CONF_SID],
        ecode=entry.data.get(CONF_ECODE, ""),
        partner=entry.data.get(CONF_PARTNER, ""),
        app_key=TUYA_APP_KEY,
        device_id=api.device_id,
        package_name=TUYA_PACKAGE_NAME,
        api_host=_entry_api_host(entry),
        uid=entry.data.get(CONF_UID, ""),
        talkback=entry.options.get(CONF_TALKBACK, DEFAULT_TALKBACK),
        bridge_port=entry.options.get(CONF_BRIDGE_PORT, DEFAULT_BRIDGE_PORT),
        cameras=cameras,
    )
    await async_configure_bridge_app(hass, options)


async def _async_options_updated(hass: HomeAssistant, entry: ConfigEntry) -> None:
    """Reload integration when options change."""
    await hass.config_entries.async_reload(entry.entry_id)


async def _setup_camera(
    hass: HomeAssistant, entry: ConfigEntry, api: PhilipsAventAPI, cam: dict
) -> tuple[str, PhilipsAventCoordinator]:
    """Bring one camera online and return it keyed by device id.

    Cameras are set up concurrently, so everything here has to be per-camera:
    the only shared state it touches is the camera dict it was handed.
    """
    cam_id = cam.get("deviceId") or cam.get("devId")
    cam_name = cam.get("deviceName") or cam.get("name", cam_id)
    local_key = cam.get("localKey")

    coordinator = PhilipsAventCoordinator(hass, entry, api, cam_id, cam_name, local_key=local_key)
    await coordinator.async_config_entry_first_refresh()

    if not local_key:
        local_key = coordinator.device_info.get("localKey")
        if local_key:
            coordinator.set_local_key(local_key)

    # The first poll already carries the local key, so keep it on the camera
    # dict for the bridge config rather than paying a second call.
    cam["localKey"] = local_key or ""
    if not cam.get("password"):
        cam["password"] = await _lan_password(api, cam_id)

    await coordinator.start_lan()
    return cam_id, coordinator


async def async_setup_entry(hass: HomeAssistant, entry: PhilipsAventConfigEntry) -> bool:
    """Set up Philips Avent from a config entry."""
    if CONF_PASSWORD in entry.data:
        # Older entries stored the account password, which nothing ever read
        # back: re-auth asks for it again. Drop it rather than keep a plaintext
        # credential in .storage.
        data = dict(entry.data)
        data.pop(CONF_PASSWORD)
        hass.config_entries.async_update_entry(entry, data=data)
        _LOGGER.info("Removed the stored account password from this entry, it was never used")

    api = PhilipsAventAPI(
        async_get_clientsession(hass),
        sid=entry.data[CONF_SID],
        api_url=api_url_for_host(_entry_api_host(entry)),
        country_code=entry.data.get(CONF_COUNTRY_CODE) or TUYA_DEFAULT_COUNTRY_CODE,
        device_id=entry.data.get(CONF_DEVICE_ID),
    )
    if not entry.data.get(CONF_DEVICE_ID):
        # Entries created before the id was persisted: keep the one just
        # generated, so the bridge config stops changing on every restart
        # (issue #73).
        hass.config_entries.async_update_entry(entry, data={**entry.data, CONF_DEVICE_ID: api.device_id})
        _LOGGER.info("Stored a stable device id for this account")

    # Use cameras stored in config entry (discovered during config flow)
    cameras = []
    stored_cameras = entry.data.get("cameras", [])
    if stored_cameras:
        cameras.extend(
            {
                "deviceId": cam["id"],
                "deviceName": cam["name"],
                "productId": cam.get("product_id", ""),
                "localKey": cam.get("local_key", ""),
                "password": cam.get("password", ""),
                "lanIp": cam.get("lan_ip", ""),
            }
            for cam in stored_cameras
        )
        _LOGGER.info("Using %d cameras from config entry", len(cameras))

        # Backfill productId for entries created before this field was tracked.
        # Guard on key presence in the stored entry, NOT on the in-memory value,
        # so post-fix entries with a genuinely empty productId (e.g. a device
        # that does not expose one) don't trigger a cloud call on every restart.
        if any("product_id" not in cam for cam in stored_cameras):
            patched = 0
            try:
                discovered = await api.discover_cameras()
                by_id = {(d.get("devId") or d.get("deviceId")): d for d in discovered}
                for cam in cameras:
                    if not cam.get("productId"):
                        disc = by_id.get(cam.get("deviceId"))
                        if disc:
                            new_id = disc.get("productId") or disc.get("productKey") or ""
                            if new_id:
                                cam["productId"] = new_id
                                patched += 1
                if patched:
                    updated_stored_cameras = [
                        {**stored_cam, "product_id": cam.get("productId", "")}
                        for stored_cam, cam in zip(stored_cameras, cameras)
                    ]
                    hass.config_entries.async_update_entry(
                        entry,
                        data={**entry.data, "cameras": updated_stored_cameras},
                    )
                    _LOGGER.info("Backfilled productId for %d camera(s) and persisted to config entry", patched)
                else:
                    _LOGGER.info("Backfill ran but no productId was recovered from Tuya discovery")
            except Exception:  # noqa: BLE001 - setup continues even if the backfill fails
                _LOGGER.warning(
                    "Could not backfill productId from Tuya API; SCD951 cameras may fail "
                    "to stream until HA restarts or the integration is reconfigured"
                )
    else:
        # Fallback: re-discover via API
        try:
            cameras = await api.discover_cameras()
        except Exception:
            _LOGGER.exception("Camera discovery failed")
            cameras = []

    if not cameras:
        _LOGGER.error("No cameras found. Reconfigure the integration to re-discover.")
        return False

    coordinators = dict(await asyncio.gather(*(_setup_camera(hass, entry, api, cam) for cam in cameras)))

    _persist_lan_secrets(hass, entry, cameras)

    entry.runtime_data = PhilipsAventData(api=api, coordinators=coordinators)

    await _configure_bridge(hass, entry, api, cameras)

    async def _publish_lan_addresses(_now) -> None:
        """Hand the bridge the address the LAN listener resolved.

        tinytuya's scan finishes well after setup, so a fresh entry only learns
        the address on this second pass. Writing it back to the entry means
        every later start has it before the first stream is asked for.
        """
        changed = False
        for cam in cameras:
            coordinator = coordinators.get(cam.get("deviceId"))
            found = coordinator.lan_ip if coordinator else ""
            if found and cam.get("lanIp") != found:
                cam["lanIp"] = found
                changed = True
        if changed:
            _persist_lan_secrets(hass, entry, cameras)
            await _configure_bridge(hass, entry, api, cameras)

    entry.async_on_unload(async_call_later(hass, LAN_ADDRESS_SETTLE, _publish_lan_addresses))

    await hass.config_entries.async_forward_entry_setups(entry, PLATFORMS)

    entry.async_on_unload(entry.add_update_listener(_async_options_updated))

    return True


async def async_unload_entry(hass: HomeAssistant, entry: PhilipsAventConfigEntry) -> bool:
    """Unload a config entry."""
    unload_ok = await hass.config_entries.async_unload_platforms(entry, PLATFORMS)

    if unload_ok:
        await asyncio.gather(*(c.stop_lan() for c in entry.runtime_data.coordinators.values()))

    return unload_ok


async def async_remove_entry(hass: HomeAssistant, entry: ConfigEntry) -> None:
    """Take this entry's credentials back out of the bridge app.

    Left in place, the app would keep streaming with a session and camera ids
    that no longer belong to any entry (issue #52).
    """
    await async_reset_bridge_app(hass)
