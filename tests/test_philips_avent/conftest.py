"""Import the integration's HA-free modules as a real package.

The unit tests exercise the pure-Python half of the integration (signing,
region routing, payload shaping) without Home Assistant installed. Registering
a stub parent package gives those modules a package to hang off, so their
relative imports resolve without ``custom_components/philips_avent/__init__.py``
- and therefore ``homeassistant`` - ever being imported.
"""

import sys
import types
from pathlib import Path

PACKAGE = "philips_avent"
SOURCE = Path(__file__).parents[2] / "custom_components" / PACKAGE

if PACKAGE not in sys.modules:
    stub = types.ModuleType(PACKAGE)
    stub.__path__ = [str(SOURCE)]
    sys.modules[PACKAGE] = stub
