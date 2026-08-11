"""Binary sensor entities for Philips Avent Baby Monitor."""

from __future__ import annotations

import logging
import time
from collections.abc import Callable

from homeassistant.components.binary_sensor import (
    BinarySensorDeviceClass,
    BinarySensorEntity,
)
from homeassistant.core import HomeAssistant, callback
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback
from homeassistant.helpers.event import async_call_later
from homeassistant.helpers.update_coordinator import CoordinatorEntity

from .const import (
    DPS_ALARM_RECORD,
    DPS_ALERT_EVENT,
    DPS_DECIBEL_EVENT,
    DPS_LULLABY_STATE,
    DPS_MOTION_SWITCH,
)
from .coordinator import PhilipsAventConfigEntry, PhilipsAventCoordinator
from .entity import build_device_info
from .events import is_new_event, motion_event_timestamp, sound_event_timestamp

_LOGGER = logging.getLogger(__name__)

ALERT_CLEAR_SECONDS = 30


async def async_setup_entry(
    hass: HomeAssistant,
    entry: PhilipsAventConfigEntry,
    async_add_entities: AddConfigEntryEntitiesCallback,
) -> None:
    coordinators = entry.runtime_data.coordinators
    entities = []
    for cam_id, coordinator in coordinators.items():
        entities.extend(
            [
                AventLullabyPlaying(coordinator, cam_id),
                AventMotionDetected(coordinator, cam_id),
                AventSoundDetected(coordinator, cam_id),
            ]
        )
    async_add_entities(entities)


class AventLullabyPlaying(CoordinatorEntity, BinarySensorEntity):
    _attr_has_entity_name = True
    _attr_name = "Lullaby Playing"
    _attr_icon = "mdi:music"
    _attr_device_class = BinarySensorDeviceClass.RUNNING

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(coordinator)
        self._cam_id = cam_id
        self._attr_unique_id = f"{cam_id}_lullaby_playing"
        self._attr_device_info = build_device_info(coordinator, cam_id)

    @property
    def is_on(self) -> bool | None:
        dps = self.coordinator.data
        if dps and DPS_LULLABY_STATE in dps:
            return dps[DPS_LULLABY_STATE] == "playing"
        return None


class AventAlertBinarySensor(CoordinatorEntity, BinarySensorEntity):
    """An alert the monitor reports, from whichever DPS its firmware uses.

    Two mechanisms, and which one a monitor uses is not predictable from the
    model (issues #40, #42, #59, #61):

    - A push DPS set to a marker value, 250 for motion and 141 for sound. It is
      an event that the coordinator merges into persistent state, so only a
      payload that arrived since the last look counts; otherwise every cloud
      poll replays the last alert (the defect fixed for sound in #65).
    - DPS 212, an alarm record carrying its own timestamp. That timestamp is
      what makes it usable: the value stays in device state, so freshness comes
      from the stamp rather than from catching the push. On an SCD973/26 this
      was the only one of the two ever populated.

    The sensor latches on and clears itself after ALERT_CLEAR_SECONDS.
    """

    _attr_has_entity_name = True

    # Subclasses declare which push value and which record parser mean "now".
    _event_dps: str
    _event_value: str
    _alarm_timestamp: Callable[[object], float | None]

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str, unique_suffix: str):
        super().__init__(coordinator)
        self._cam_id = cam_id
        self._attr_unique_id = f"{cam_id}_{unique_suffix}"
        self._attr_device_info = build_device_info(coordinator, cam_id)
        self._is_on = False
        self._clear_unsub = None
        self._last_lan_update_sequence = coordinator.lan_update_sequence
        self._last_alarm_timestamp: float | None = None

    @property
    def is_on(self) -> bool:
        return self._is_on

    def _alert_enabled(self) -> bool:
        """Whether the monitor is set to raise this alert at all."""
        return True

    @callback
    def _handle_coordinator_update(self) -> None:
        if self._alert_reported():
            self._is_on = True
            self._schedule_clear()
        super()._handle_coordinator_update()

    @callback
    def _alert_reported(self) -> bool:
        if not self._alert_enabled():
            return False

        sequence = self.coordinator.lan_update_sequence
        if sequence != self._last_lan_update_sequence:
            self._last_lan_update_sequence = sequence
            if self.coordinator.last_lan_dps.get(self._event_dps) == self._event_value:
                return True

        timestamp = self._alarm_timestamp((self.coordinator.data or {}).get(DPS_ALARM_RECORD))
        if is_new_event(timestamp, self._last_alarm_timestamp, time.time()):
            self._last_alarm_timestamp = timestamp
            _LOGGER.debug("%s alarm record for %s at %s", self._attr_name, self.coordinator.camera_name, timestamp)
            return True

        # Remember a stale record so it cannot fire later as if it were new.
        if timestamp is not None and self._last_alarm_timestamp is None:
            self._last_alarm_timestamp = timestamp
        return False

    @callback
    def _schedule_clear(self) -> None:
        if self._clear_unsub:
            self._clear_unsub()
        self._clear_unsub = async_call_later(self.hass, ALERT_CLEAR_SECONDS, self._clear_alert)

    @callback
    def _clear_alert(self, _now=None) -> None:
        self._is_on = False
        self._clear_unsub = None
        self.async_write_ha_state()

    async def async_will_remove_from_hass(self) -> None:
        if self._clear_unsub:
            self._clear_unsub()
            self._clear_unsub = None
        await super().async_will_remove_from_hass()


class AventMotionDetected(AventAlertBinarySensor):
    _attr_name = "Motion Detected"
    _attr_device_class = BinarySensorDeviceClass.MOTION

    _event_dps = DPS_ALERT_EVENT
    _event_value = "motion_detection"
    _alarm_timestamp = staticmethod(motion_event_timestamp)

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(coordinator, cam_id, "motion_detected")

    def _alert_enabled(self) -> bool:
        # Absent means the monitor does not expose the toggle, so assume on.
        return (self.coordinator.data or {}).get(DPS_MOTION_SWITCH, True)


class AventSoundDetected(AventAlertBinarySensor):
    _attr_name = "Sound Detected"
    _attr_device_class = BinarySensorDeviceClass.SOUND

    _event_dps = DPS_DECIBEL_EVENT
    _event_value = "decibel_upload"
    # The SCD953 names a noise alert `ipc_bang`, confirmed by diagnostics on #42.
    _alarm_timestamp = staticmethod(sound_event_timestamp)

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(coordinator, cam_id, "sound_detected")
