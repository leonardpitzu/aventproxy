"""Diagnostics for Philips Avent Baby Monitor."""

from __future__ import annotations

from typing import Any

from homeassistant.components.diagnostics import async_redact_data
from homeassistant.core import HomeAssistant

from .coordinator import PhilipsAventConfigEntry

# `cameras` is a list of dicts, so the redactor has to walk lists as well as
# dicts: local_key and password are the monitor's LAN streaming credentials.
REDACT_KEYS = {
    "sid",
    "ecode",
    "uid",
    "partner_identity",
    "localKey",
    "local_key",
    "password",
    "email",
}


async def async_get_config_entry_diagnostics(hass: HomeAssistant, entry: PhilipsAventConfigEntry) -> dict[str, Any]:
    """Return diagnostics for a config entry."""
    diag: dict[str, Any] = {
        "config_entry": async_redact_data(dict(entry.data), REDACT_KEYS),
        "devices": {},
    }

    for cam_id, coordinator in entry.runtime_data.coordinators.items():
        diag["devices"][cam_id] = {
            "name": coordinator.camera_name,
            "dps": coordinator.data,
            "lan_connected": coordinator.lan_connected,
            "update_interval": str(coordinator.update_interval),
            "rssi": coordinator.rssi,
            "device_info": async_redact_data(coordinator.device_info, REDACT_KEYS) if coordinator.device_info else None,
        }

    return diag
