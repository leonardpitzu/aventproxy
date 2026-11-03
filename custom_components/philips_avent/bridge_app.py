"""Hand the bridge app its settings through the Supervisor.

The config entry holds the credentials. The bridge app gets a copy as its
options, which the Supervisor writes to the app's private /data/options.json
before each start, so nothing is written to the Home Assistant config folder
and the app maps no host folders. The Z-Wave JS integration and its app work
the same way.
"""

from __future__ import annotations

import asyncio
import logging
from dataclasses import dataclass, field
from hashlib import sha1
from urllib.parse import urlencode

from aiohasupervisor import SupervisorError
from homeassistant.components.hassio import AddonError, AddonManager, AddonState, get_supervisor_client
from homeassistant.core import HomeAssistant
from homeassistant.helpers import issue_registry as ir
from homeassistant.helpers.hassio import is_hassio
from homeassistant.util.hass_dict import HassKey

from .const import DOMAIN
from .payload import bridge_app_action

_LOGGER = logging.getLogger(__name__)

APP_NAME = "Philips Avent WebRTC Bridge"
APP_REPOSITORY = "https://github.com/leonardpitzu/aventproxy"
APP_SLUG = "aventproxy_bridge"
ISSUE_APP_MISSING = "bridge_app_missing"


@dataclass
class _BridgeApp:
    """Shared by every entry: there is one app, so one manager and one lock."""

    lock: asyncio.Lock = field(default_factory=asyncio.Lock)
    manager: AddonManager | None = None


DATA_BRIDGE_APP: HassKey[_BridgeApp] = HassKey(f"{DOMAIN}_bridge_app")


def _bridge_app(hass: HomeAssistant) -> _BridgeApp:
    return hass.data.setdefault(DATA_BRIDGE_APP, _BridgeApp())


def _store_slug() -> str:
    """The slug the Supervisor gives the app when installed from APP_REPOSITORY."""
    return f"{sha1(APP_REPOSITORY.lower().encode()).hexdigest()[:8]}_{APP_SLUG}"


async def _async_manager(hass: HomeAssistant, bridge: _BridgeApp) -> AddonManager | None:
    """The manager for the installed bridge app, or None if it is not installed.

    The Supervisor prefixes a custom app's slug with a hash of the repository
    URL, so a fork or a local copy has a different one. Matching the installed
    apps on the suffix finds any of them.
    """
    if bridge.manager is None:
        installed = await get_supervisor_client(hass).addons.list()
        slug = next((app.slug for app in installed if app.slug.endswith(f"_{APP_SLUG}")), None)
        if slug is None:
            return None
        bridge.manager = AddonManager(hass, _LOGGER, APP_NAME, slug)
    return bridge.manager


def _report_missing(hass: HomeAssistant) -> None:
    query = urlencode({"addon": _store_slug(), "repository_url": APP_REPOSITORY})
    ir.async_create_issue(
        hass,
        DOMAIN,
        ISSUE_APP_MISSING,
        is_fixable=False,
        severity=ir.IssueSeverity.WARNING,
        translation_key=ISSUE_APP_MISSING,
        learn_more_url=f"https://my.home-assistant.io/redirect/supervisor_addon/?{query}",
    )


async def async_configure_bridge_app(hass: HomeAssistant, options: dict) -> None:
    """Make the bridge app run with `options`, restarting it only if they changed.

    Without a Supervisor there is no app to configure: the bridge is the user's
    own process. Failures are logged rather than raised, because everything
    except the video works without the bridge.
    """
    if not is_hassio(hass):
        return

    bridge = _bridge_app(hass)
    async with bridge.lock:
        try:
            manager = await _async_manager(hass, bridge)
            if manager is None:
                _LOGGER.warning("The %s app is not installed, so the cameras have no video", APP_NAME)
                _report_missing(hass)
                return
            ir.async_delete_issue(hass, DOMAIN, ISSUE_APP_MISSING)

            info = await manager.async_get_addon_info()
            action = bridge_app_action(info.options, options, running=info.state is AddonState.RUNNING)
            if action is None:
                return

            await manager.async_set_addon_options(options)
            if action == "restart":
                await manager.async_restart_addon()
            else:
                await manager.async_start_addon()
            _LOGGER.info("Bridge app options updated, app %sed", action)
        except (AddonError, SupervisorError) as err:
            _LOGGER.warning("Could not configure the %s app: %s", APP_NAME, err)


async def async_reset_bridge_app(hass: HomeAssistant) -> None:
    """Stop the bridge app and take the removed entry's credentials out of it."""
    if not is_hassio(hass):
        return

    bridge = _bridge_app(hass)
    async with bridge.lock:
        ir.async_delete_issue(hass, DOMAIN, ISSUE_APP_MISSING)
        try:
            manager = await _async_manager(hass, bridge)
            if manager is None:
                return
            info = await manager.async_get_addon_info()
            if info.state is AddonState.RUNNING:
                await manager.async_stop_addon()
            await manager.async_set_addon_options({})
            _LOGGER.info("Bridge app stopped and its options cleared")
        except (AddonError, SupervisorError) as err:
            _LOGGER.warning("Could not reset the %s app: %s", APP_NAME, err)
