"""Data update coordinator for Philips Avent."""

from __future__ import annotations

import logging
import time
from dataclasses import dataclass
from datetime import timedelta
from typing import Any

from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant, callback
from homeassistant.exceptions import ConfigEntryAuthFailed
from homeassistant.helpers.event import async_call_later
from homeassistant.helpers.update_coordinator import DataUpdateCoordinator, UpdateFailed

from .api import PhilipsAventAPI, TuyaAPIError, is_auth_error
from .const import DPS_ALARM_RECORD, DPS_LULLABY_CONTROL, DPS_LULLABY_STATE
from .events import LULLABY_SETTLE_SECONDS, cloud_poll_needed, lullaby_state_settled
from .lan import TuyaLANClient
from .payload import dps_delta, truncated_dps

LULLABY_STATE_MAP = {"play": "playing", "stop": "stopping"}

_LOGGER = logging.getLogger(__name__)

POLL_FAST = timedelta(seconds=30)
POLL_SLOW = timedelta(seconds=120)


@dataclass
class PhilipsAventData:
    """What a set-up config entry hands to its platforms."""

    api: PhilipsAventAPI
    coordinators: dict[str, PhilipsAventCoordinator]


type PhilipsAventConfigEntry = ConfigEntry[PhilipsAventData]


class PhilipsAventCoordinator(DataUpdateCoordinator[dict[str, Any]]):
    """Polls camera DPS values, with optional LAN push for real-time updates."""

    def __init__(
        self,
        hass: HomeAssistant,
        entry: ConfigEntry,
        api: PhilipsAventAPI,
        camera_id: str,
        camera_name: str,
        local_key: str | None = None,
    ):
        super().__init__(
            hass,
            _LOGGER,
            config_entry=entry,
            name=f"Philips Avent {camera_name}",
            update_interval=POLL_FAST,
        )
        self.api = api
        self.camera_id = camera_id
        self.camera_name = camera_name
        self.device_info: dict = {}
        self._local_key = local_key
        self._lan_client: TuyaLANClient | None = None
        self.last_lan_dps: dict[str, Any] = {}
        self.lan_update_sequence = 0
        self._pending_lullaby: str | None = None
        self._pending_lullaby_since: float | None = None
        self._lullaby_unsub = None

    async def start_lan(self) -> None:
        if not self._local_key:
            return
        self._lan_client = TuyaLANClient(
            self.hass,
            self.camera_id,
            self._local_key,
            self._on_lan_dps_update,
        )
        await self._lan_client.start()

    def set_local_key(self, local_key: str) -> None:
        """Adopt a local key learned after construction, before the LAN starts."""
        self._local_key = local_key

    async def stop_lan(self) -> None:
        self._cancel_pending_lullaby()
        if self._lan_client:
            await self._lan_client.stop()
            self._lan_client = None

    @property
    def lan_connected(self) -> bool:
        return self._lan_client is not None and self._lan_client.connected

    @property
    def lan_ip(self) -> str:
        """Monitor's address, so the bridge need not repeat the discovery."""
        return (self._lan_client.ip if self._lan_client else None) or ""

    @property
    def lan_protocol_version(self) -> float | None:
        """Protocol the live LAN session negotiated, which decides what it can push."""
        return self._lan_client.protocol_version if self._lan_client else None

    @callback
    def _on_lan_dps_update(self, dps: dict[str, Any], baseline: bool = False) -> None:
        """Merge DPS the LAN session reported into entity state.

        A baseline is the answer to a DP_QUERY sent when the session comes up, so
        it is state and not news: recording it as a push would let a retained
        alert value fire the motion or sound sensor on every reconnect.
        """
        if self.data is None:
            return
        if not baseline:
            self.last_lan_dps = dict(dps)
            self.lan_update_sequence += 1
        _LOGGER.debug(
            "LAN %s for %s: %s",
            "baseline" if baseline else "push",
            self.camera_name,
            truncated_dps(dps),
        )

        dps = self._hold_lullaby_state(dps)
        merged = {**self.data, **dps}
        self.async_set_updated_data(merged)

    @callback
    def _hold_lullaby_state(self, dps: dict[str, Any]) -> dict[str, Any]:
        """Keep a lullaby state out of entity state until it stands still.

        The camera re-announces `stopping` and then `playing` within a third of a
        second when a viewing session ends, which reached the Lullaby Playing
        sensor as a flicker (issue #72). Holding the value lets such a pair
        cancel itself.
        """
        if DPS_LULLABY_STATE not in dps:
            return dps

        value = dps[DPS_LULLABY_STATE]
        rest = {k: v for k, v in dps.items() if k != DPS_LULLABY_STATE}

        if value == (self.data or {}).get(DPS_LULLABY_STATE):
            # Already the state on show; nothing to hold or cancel.
            self._cancel_pending_lullaby()
            return rest

        self._pending_lullaby = value
        self._pending_lullaby_since = time.monotonic()
        if self._lullaby_unsub:
            self._lullaby_unsub()
        self._lullaby_unsub = async_call_later(self.hass, LULLABY_SETTLE_SECONDS, self._commit_lullaby_state)
        _LOGGER.debug(
            "Holding lullaby state %r for %s until it settles",
            value,
            self.camera_name,
        )
        return rest

    @callback
    def _cancel_pending_lullaby(self) -> None:
        if self._lullaby_unsub:
            self._lullaby_unsub()
            self._lullaby_unsub = None
        self._pending_lullaby = None
        self._pending_lullaby_since = None

    @callback
    def _commit_lullaby_state(self, _now=None) -> None:
        """Apply a held lullaby state once it has stood still."""
        self._lullaby_unsub = None
        if not lullaby_state_settled(self._pending_lullaby, self._pending_lullaby_since, time.monotonic()):
            return
        value = self._pending_lullaby
        self._pending_lullaby = None
        self._pending_lullaby_since = None
        if self.data is None or value is None:
            return
        _LOGGER.debug("Lullaby state settled at %r for %s", value, self.camera_name)
        self.async_set_updated_data({**self.data, DPS_LULLABY_STATE: value})

    @callback
    def _apply_optimistic(self, dps: dict, extra: dict | None = None) -> None:
        """Show a command's effect before the monitor confirms it."""
        if self.data is None:
            return
        optimistic = {str(k): v for k, v in dps.items()}
        lullaby_cmd = optimistic.get(DPS_LULLABY_CONTROL)
        if lullaby_cmd in LULLABY_STATE_MAP:
            optimistic[DPS_LULLABY_STATE] = LULLABY_STATE_MAP[lullaby_cmd]
        if extra:
            optimistic.update(extra)
        self.async_set_updated_data({**self.data, **optimistic})

    async def set_dps(self, dps: dict, *, optimistic: dict | None = None) -> dict:
        """Send a DPS command, over the LAN when there is a session for it.

        A command that reached the monitor locally is done: the monitor reports
        the change onwards itself, so repeating it through the cloud only sends
        the same write out over the internet. The cloud is the path for a
        monitor we cannot reach locally, not a second copy of every press.

        `optimistic` carries values the monitor reports back under a different
        key than the one written, such as the lullaby track in DPS 248.
        """
        if self._lan_client and self._lan_client.connected:
            result = await self._lan_client.set_dps(dps)
            if result:
                _LOGGER.debug("DPS sent via LAN for %s: %s", self.camera_name, dps)
                self._apply_optimistic(dps, optimistic)
                return result
        self._apply_optimistic(dps, optimistic)
        return await self.api.set_dps(self.camera_id, dps)

    @property
    def alerts_need_the_cloud(self) -> bool:
        """Whether this monitor reports alarms in the record DPS.

        Alarms land in DPS 212 as a timestamped record on some firmwares and in
        250 and 141 as marker values on others, and the model does not say
        which: an SCD973/26 used 212 exclusively while both marker keys stayed
        empty. 212 is also a single slot holding the newest alarm rather than a
        queue, so a second alert overwrites the first and anything the session
        misses is gone. Whether the session can carry it at all depends on the
        protocol version, which is what decides the poll - see
        events.cloud_poll_needed.
        """
        return DPS_ALARM_RECORD in (self.data or {})

    async def _async_update_data(self) -> dict:
        needs_cloud = cloud_poll_needed(self.lan_connected, self.alerts_need_the_cloud, self.lan_protocol_version)
        self.update_interval = POLL_FAST if needs_cloud else POLL_SLOW

        # The first refresh runs before the LAN session has a baseline, and it
        # is what fills device_info for the device registry.
        if self.data is not None and not needs_cloud:
            return self.data

        try:
            device = await self.api.get_device(self.camera_id)
            self.device_info = device
            api_dps = device.get("dps", {})
            changed = dps_delta(self.data, api_dps)
            if changed:
                _LOGGER.debug("Cloud poll changed DPS for %s: %s", self.camera_name, changed)
            if self.data:
                return {**self.data, **api_dps}
            return api_dps
        except TuyaAPIError as err:
            if is_auth_error(err):
                raise ConfigEntryAuthFailed(f"Authentication failed: {err}") from err
            raise UpdateFailed(f"Error fetching data: {err}") from err
