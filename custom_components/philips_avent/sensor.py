"""Sensor entities for Philips Avent Baby Monitor."""

from __future__ import annotations

import time
from datetime import UTC, datetime

from homeassistant.components.sensor import (
    SensorDeviceClass,
    SensorEntity,
    SensorStateClass,
)
from homeassistant.const import UnitOfTemperature, UnitOfTime
from homeassistant.core import HomeAssistant, callback
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback
from homeassistant.helpers.update_coordinator import CoordinatorEntity

from .const import DPS_SLEEP_SESSION, DPS_TEMPERATURE
from .coordinator import PhilipsAventConfigEntry, PhilipsAventCoordinator
from .entity import build_device_info
from .senseiq import SLEEP_STAGES, SleepSession, occupied_reading, parse_sleep_session


async def async_setup_entry(
    hass: HomeAssistant,
    entry: PhilipsAventConfigEntry,
    async_add_entities: AddConfigEntryEntitiesCallback,
) -> None:
    coordinators = entry.runtime_data.coordinators
    entities = []
    for cam_id, coordinator in coordinators.items():
        entities.append(AventTemperatureSensor(coordinator, cam_id))
        if coordinator.has_senseiq:
            entities.extend(
                [
                    AventBreathingRate(coordinator, cam_id),
                    AventSleepStage(coordinator, cam_id),
                    AventSleepTime(coordinator, cam_id),
                    AventInBedSince(coordinator, cam_id),
                ]
            )
    async_add_entities(entities)


class AventTemperatureSensor(CoordinatorEntity, SensorEntity):
    _attr_device_class = SensorDeviceClass.TEMPERATURE
    _attr_state_class = SensorStateClass.MEASUREMENT
    _attr_native_unit_of_measurement = UnitOfTemperature.CELSIUS
    _attr_has_entity_name = True
    _attr_name = "Temperature"

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(coordinator)
        self._cam_id = cam_id
        self._attr_unique_id = f"{cam_id}_temperature"
        self._attr_device_info = build_device_info(coordinator, cam_id)

    @property
    def native_value(self) -> float | None:
        dps = self.coordinator.data
        if dps and DPS_TEMPERATURE in dps:
            return dps[DPS_TEMPERATURE] / 100.0
        return None


class AventSenseIQSensor(CoordinatorEntity, SensorEntity):
    """SenseIQ data, which only ever arrives over the LAN session."""

    _attr_has_entity_name = True

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str, unique_suffix: str):
        super().__init__(coordinator)
        self._cam_id = cam_id
        self._attr_unique_id = f"{cam_id}_{unique_suffix}"
        self._attr_device_info = build_device_info(coordinator, cam_id)

    @property
    def available(self) -> bool:
        return super().available and self.coordinator.lan_connected

    def _session(self) -> SleepSession | None:
        return parse_sleep_session((self.coordinator.data or {}).get(DPS_SLEEP_SESSION))


class AventBreathingRate(AventSenseIQSensor):
    """Breaths per minute.

    The camera measures only while the baby lies still and sends 0 while it
    moves, so the last measured rate is held through movement rather than
    dropping to unknown every few seconds. It clears once the crib is empty.
    """

    _attr_name = "Breathing Rate"
    _attr_icon = "mdi:lungs"
    _attr_native_unit_of_measurement = "breaths/min"
    _attr_state_class = SensorStateClass.MEASUREMENT

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(coordinator, cam_id, "breathing_rate")
        self._update_rate()

    @callback
    def _handle_coordinator_update(self) -> None:
        self._update_rate()
        super()._handle_coordinator_update()

    @callback
    def _update_rate(self) -> None:
        reading = occupied_reading(self.coordinator.breathing, self.coordinator.breathing_at, time.monotonic())
        if reading is None:
            self._attr_native_value = None
        elif reading.breathing_rate:
            self._attr_native_value = reading.breathing_rate


class AventSleepStage(AventSenseIQSensor):
    _attr_name = "Sleep Stage"
    # For the state labels in strings.json; the name above still wins.
    _attr_translation_key = "sleep_stage"
    _attr_icon = "mdi:sleep"
    _attr_device_class = SensorDeviceClass.ENUM
    # The whole session rides along for dashboards; recording it every minute
    # would only repeat the stage history the state already keeps.
    _unrecorded_attributes = frozenset({"stage_duration", "session_duration", "history"})

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(coordinator, cam_id, "sleep_stage")
        self._attr_options = list(SLEEP_STAGES.values())

    @property
    def native_value(self) -> str | None:
        session = self._session()
        return session.stage if session else None

    @property
    def extra_state_attributes(self) -> dict | None:
        session = self._session()
        if session is None:
            return None
        return {
            "stage_duration": session.stage_duration,
            "session_duration": session.duration,
            "history": [{"stage": stage, "seconds": seconds} for stage, seconds in session.history],
        }


class AventSleepTime(AventSenseIQSensor):
    """Light plus deep sleep in the current session.

    Not the session length the camera reports, which keeps counting while the
    baby is awake or out of bed.
    """

    _attr_name = "Sleep Time"
    _attr_icon = "mdi:bed-clock"
    _attr_device_class = SensorDeviceClass.DURATION
    _attr_native_unit_of_measurement = UnitOfTime.SECONDS
    _attr_suggested_unit_of_measurement = UnitOfTime.MINUTES

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(coordinator, cam_id, "sleep_time")

    @property
    def native_value(self) -> int | None:
        session = self._session()
        return session.asleep if session else None


class AventInBedSince(AventSenseIQSensor):
    _attr_name = "In Bed Since"
    _attr_icon = "mdi:bed"
    _attr_device_class = SensorDeviceClass.TIMESTAMP

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(coordinator, cam_id, "in_bed_since")

    @property
    def native_value(self) -> datetime | None:
        session = self._session()
        if session is None or not session.started:
            return None
        return datetime.fromtimestamp(session.started, tz=UTC)
