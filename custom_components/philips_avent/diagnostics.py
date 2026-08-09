"""Diagnostics for Philips Avent Baby Monitor."""

from __future__ import annotations

from typing import Any

from homeassistant.core import HomeAssistant

from .coordinator import PhilipsAventConfigEntry

REDACT_KEYS = {"sid", "ecode", "uid", "partner_identity", "localKey", "local_key", "password", "email"}


def _redact(data: dict, keys: set) -> dict:
    """Recursively redact sensitive keys from a dictionary."""
    return {
        k: "**REDACTED**" if k in keys else (_redact(v, keys) if isinstance(v, dict) else v) for k, v in data.items()
    }


async def async_get_config_entry_diagnostics(hass: HomeAssistant, entry: PhilipsAventConfigEntry) -> dict[str, Any]:
    """Return diagnostics for a config entry."""
    diag: dict[str, Any] = {
        "config_entry": _redact(dict(entry.data), REDACT_KEYS),
        "devices": {},
    }

    for cam_id, coordinator in entry.runtime_data.coordinators.items():
        diag["devices"][cam_id] = {
            "name": coordinator.camera_name,
            "dps": coordinator.data,
            "lan_connected": coordinator.lan_connected,
            "update_interval": str(coordinator.update_interval),
            "rssi": coordinator.rssi,
            "device_info": _redact(coordinator.device_info, REDACT_KEYS) if coordinator.device_info else None,
        }

    return diag
