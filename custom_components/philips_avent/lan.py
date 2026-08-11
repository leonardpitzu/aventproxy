"""Tuya LAN protocol client for real-time DPS push updates."""

from __future__ import annotations

import asyncio
import contextlib
import logging
import time
from collections.abc import Callable
from typing import Any

import tinytuya
from homeassistant.core import HomeAssistant

from .lan_policy import (
    PROTOCOL_VERSION_DEFAULT,
    data_stale,
    heartbeat_due,
    is_connection_error,
    parse_protocol_version,
    reconnect_delay,
    should_reconnect,
    version_candidates,
)

_LOGGER = logging.getLogger(__name__)

SOCKET_TIMEOUT = 5
SCAN_MAXRETRY = 5


class TuyaLANClient:
    """Persistent LAN connection to a Tuya device for real-time DPS push."""

    def __init__(
        self,
        hass: HomeAssistant,
        device_id: str,
        local_key: str,
        on_dps_update: Callable[[dict[str, Any], bool], None],
    ) -> None:
        self._hass = hass
        self._device_id = device_id
        self._local_key = local_key
        self._on_dps_update = on_dps_update
        self._device: tinytuya.Device | None = None
        self._ip: str | None = None
        self._announced_version: float | None = None
        self._version: float | None = None
        self._task: asyncio.Task | None = None
        self._stop_event = asyncio.Event()
        self.connected: bool = False

    async def start(self) -> None:
        self._stop_event.clear()
        self._task = self._hass.async_create_background_task(self._run(), "tuya_lan_listener")

    @property
    def ip(self) -> str | None:
        """Address of the monitor, once a LAN session has resolved one."""
        return self._ip

    @property
    def protocol_version(self) -> float | None:
        """Protocol the live session negotiated, or None when there is no session."""
        return self._version if self.connected else None

    async def stop(self) -> None:
        self._stop_event.set()
        if self._task and not self._task.done():
            self._task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await self._task
        self._disconnect()

    def _disconnect(self) -> None:
        if self._device:
            try:
                self._device.close()
            except Exception as ex:  # noqa: BLE001 - teardown must not raise, whatever tinytuya throws
                _LOGGER.debug("Ignoring error while closing the LAN socket: %s", ex)
            self._device = None
        self.connected = False

    async def _discover_device(self) -> tuple[str | None, float | None]:
        """Find the monitor on the LAN, keeping the protocol version it announces."""

        def _scan():
            devices = tinytuya.deviceScan(maxretry=SCAN_MAXRETRY)
            for ip, info in devices.items():
                if info.get("gwId") == self._device_id:
                    return ip, parse_protocol_version(info.get("version"))
            return None, None

        return await self._hass.async_add_executor_job(_scan)

    def _try_direct_connect(self, ip: str, version: float) -> tinytuya.Device | None:
        """Try connecting directly to a known IP via TCP handshake only.

        Don't send updatedps() — it crashes this firmware. DP_QUERY is safe and
        is sent once the session is up, see `_query_baseline`.

        `_get_socket` answers True or a tinytuya error code, and from 3.4 up that
        covers the session-key negotiation, so the caller learns whether this
        protocol version actually works on this firmware.
        """
        try:
            d = tinytuya.Device(self._device_id, ip, self._local_key, version=version)
            d.set_socketPersistent(True)
            d.set_socketTimeout(SOCKET_TIMEOUT)
            result = d._get_socket(False)
            if result is True:
                return d
            _LOGGER.debug("LAN connection to %s at protocol %s refused: %s", ip, version, result)
            d.close()
        except Exception as ex:  # noqa: BLE001 - tinytuya raises socket, decode and key errors here
            _LOGGER.debug("Direct LAN connection to %s at protocol %s failed: %s", ip, version, ex)
        return None

    async def _connect_at_any_version(self, ip: str) -> tinytuya.Device | None:
        """Connect to a known IP, trying the announced protocol then the default."""
        for version in version_candidates(self._version or self._announced_version):
            device = await self._hass.async_add_executor_job(self._try_direct_connect, ip, version)
            if device:
                if version != self._version:
                    _LOGGER.info("LAN session with %s speaks protocol %s", self._device_id, version)
                self._version = version
                return device
        return None

    async def _connect(self) -> bool:
        # Try cached IP first (direct TCP, no broadcast needed)
        if self._ip:
            _LOGGER.debug("Trying direct LAN connection to %s at %s", self._device_id, self._ip)
            device = await self._connect_at_any_version(self._ip)
            if device:
                self._device = device
                self.connected = True
                _LOGGER.info("LAN reconnected to %s at %s", self._device_id, self._ip)
                return True
            self._ip = None

        # Scan for device IP
        _LOGGER.debug("Scanning LAN for device %s", self._device_id)
        self._ip, announced = await self._discover_device()
        if announced is not None and announced != self._announced_version:
            _LOGGER.debug("Device %s announces protocol %s", self._device_id, announced)
            self._announced_version = announced
        if not self._ip:
            _LOGGER.warning("Device %s not found on LAN", self._device_id)
            return False

        device = await self._connect_at_any_version(self._ip)
        if device:
            self._device = device
            self.connected = True
            _LOGGER.info("LAN connected to %s at %s", self._device_id, self._ip)
            return True

        _LOGGER.debug("LAN connection failed for %s at %s", self._device_id, self._ip)
        self._ip = None
        return False

    def _query_baseline(self) -> dict[str, Any] | None:
        """Read the monitor's whole DPS set on the session that just came up.

        Measured against a live SCD973/26 on a 3.5 session: 37 keys, identical to
        what the cloud reported at the same moment. Older sessions answer with an
        error frame instead, which costs nothing — the cloud poll still carries
        the baseline, and this only ever runs once per connection.
        """
        try:
            result = self._device.status()
        except Exception as ex:  # noqa: BLE001 - tinytuya raises socket and decode errors alike
            _LOGGER.debug("LAN baseline query failed: %s", ex)
            return None

        if not isinstance(result, dict) or not result.get("dps"):
            _LOGGER.debug("LAN baseline query returned no state: %s", result)
            return None
        return result["dps"]

    async def _load_baseline(self) -> None:
        """Publish the full local state, so it need not wait for a cloud poll."""
        dps = await self._hass.async_add_executor_job(self._query_baseline)
        if not dps:
            return
        _LOGGER.debug("LAN baseline for %s carries %d keys", self._device_id, len(dps))
        self._publish(dps, baseline=True)

    def _publish(self, dps: dict[str, Any], *, baseline: bool = False) -> None:
        """Hand pushed state to the coordinator, on the event loop."""
        try:
            self._on_dps_update(dps, baseline)
        except Exception:
            _LOGGER.exception("Error in DPS %s callback", "baseline" if baseline else "update")

    def _send_heartbeat(self) -> tuple[bool, dict[str, Any] | None]:
        """Ping the device on the persistent socket.

        Returns whether the socket is still usable, and any state that came back
        with the ack. tinytuya does not keep persistent sockets alive by itself,
        so without this the monitor closes the connection after 30-45 seconds of
        silence (issue #62).

        The ack has to be consumed here (`nowait=False`) or a stray one turns up
        in the next receive() and looks like a device error. That read takes
        whatever frame arrives first, so an alert the monitor pushed into this
        window comes back in place of the ack: measured on an SCD973/26, roughly
        half of a run of alarms reached Home Assistant only through the cloud
        poll while this returned a bare True and dropped the payload. Hence the
        second element - the caller publishes it on the event loop, where the
        callback belongs.

        Only a connection error counts as failure: an ack we cannot parse still
        proves the socket is alive.
        """
        try:
            result = self._device.heartbeat(nowait=False)
        except Exception as ex:  # noqa: BLE001 - any failure here means the socket is unusable
            _LOGGER.debug("LAN heartbeat failed (%s)", ex)
            return False, None

        if not isinstance(result, dict):
            return True, None
        if is_connection_error(result.get("Err")):
            _LOGGER.debug("LAN heartbeat unreachable: %s", result.get("Error"))
            return False, None
        return True, result.get("dps") or None

    async def _run(self) -> None:
        last_data = time.monotonic()
        last_heartbeat = last_data
        failures = 0
        payload_errors = 0

        while not self._stop_event.is_set():
            if not self._device:
                if not await self._connect():
                    failures += 1
                    await self._interruptible_sleep(reconnect_delay(failures))
                    continue
                await self._load_baseline()
                last_data = time.monotonic()
                last_heartbeat = last_data
                payload_errors = 0

            if data_stale(time.monotonic(), last_data):
                _LOGGER.debug("No LAN data within the timeout, reconnecting")
                self._disconnect()
                continue

            try:
                data = await self._hass.async_add_executor_job(self._device.receive)
            except Exception as ex:  # noqa: BLE001 - tinytuya raises socket and decode errors alike
                _LOGGER.debug("LAN receive error (%s), reconnecting", ex)
                self._disconnect()
                failures += 1
                await self._interruptible_sleep(reconnect_delay(failures))
                continue

            if data and isinstance(data, dict):
                last_data = time.monotonic()

                if "Error" in data or "Err" in data:
                    payload_errors += 1
                    err_code = data.get("Err")
                    if should_reconnect(err_code, payload_errors):
                        _LOGGER.debug(
                            "LAN device error: %s, reconnecting",
                            data.get("Error", err_code),
                        )
                        self._disconnect()
                        failures += 1
                        await self._interruptible_sleep(reconnect_delay(failures))
                        continue
                    _LOGGER.debug(
                        "Ignoring LAN frame we could not read (%s), connection still up",
                        data.get("Error", err_code),
                    )
                else:
                    # A real exchange: the connection is healthy.
                    failures = 0
                    payload_errors = 0

                    if data.get("dps"):
                        self._publish(data["dps"])

            # Keep-alive. Runs after the receive so the two never share the
            # socket at the same time.
            now = time.monotonic()
            if heartbeat_due(now, last_heartbeat):
                alive, pushed = await self._hass.async_add_executor_job(self._send_heartbeat)
                if not alive:
                    self._disconnect()
                    failures += 1
                    await self._interruptible_sleep(reconnect_delay(failures))
                    continue
                if pushed:
                    self._publish(pushed)
                last_heartbeat = now
                last_data = now
                failures = 0

    async def set_dps(self, dps: dict) -> dict | None:
        """Send DPS command via a temporary LAN connection.

        Uses a separate short-lived socket so the persistent listener
        is never disrupted. Sends each value individually — the device
        ignores batched commands for certain codes (e.g. 201 + 202).
        """
        if not self._ip:
            return None

        def _send():
            d = tinytuya.Device(
                self._device_id,
                self._ip,
                self._local_key,
                version=self._version or PROTOCOL_VERSION_DEFAULT,
            )
            d.set_socketTimeout(SOCKET_TIMEOUT)
            try:
                for key, value in dps.items():
                    d.set_value(key, value)
            finally:
                d.close()

        try:
            await self._hass.async_add_executor_job(_send)
            return {"success": True}
        except Exception as ex:  # noqa: BLE001 - a failed command falls back to the cloud path
            _LOGGER.warning("LAN set_dps failed: %s", ex)
            return None

    async def _interruptible_sleep(self, seconds: float) -> None:
        with contextlib.suppress(TimeoutError):
            await asyncio.wait_for(self._stop_event.wait(), timeout=seconds)
