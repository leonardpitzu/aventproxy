#!/usr/bin/env python3
"""Fetch a camera's Tuya localKey and write it to a file, never to the screen.

Credentials are prompted for interactively and are not echoed or logged. The
key lands in --out (default /tmp/avent.key) with 0600 permissions, ready for
tools/lan302_decode.py to pick up.

    tools/fetch_local_key.py --device-id bfc4beffe9d8009b8fpguq
"""
from __future__ import annotations

import argparse
import ast
import getpass
import os
import pathlib
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent
sys.path.insert(0, str(REPO / "examples"))

from tuya_client import TuyaAPIError, TuyaClient  # noqa: E402


def const(name: str) -> str:
    """Read a string constant out of const.py without importing Home Assistant."""
    tree = ast.parse((REPO / "custom_components" / "philips_avent" / "const.py").read_text())
    for node in tree.body:
        if isinstance(node, ast.Assign) and any(
            isinstance(t, ast.Name) and t.id == name for t in node.targets
        ):
            return ast.literal_eval(node.value)
    raise SystemExit(f"{name} not found in const.py")


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--device-id", required=True)
    ap.add_argument("--out", default="/tmp/avent.key")
    ap.add_argument("--country", default="40", help="phone country code (40 = RO)")
    args = ap.parse_args()

    client = TuyaClient(signing_key=const("TUYA_SIGNING_KEY"), app_key=const("TUYA_APP_KEY"))

    email = input("Avent app email: ").strip()
    password = getpass.getpass("Avent app password (not echoed): ")

    try:
        client.login(email, password, args.country, mfa_code="")
    except TuyaAPIError as err:
        if err.code != "MFA_NEED_SEND_CODE":
            raise SystemExit(f"login failed: {err}") from err
        client.trigger_mfa(email, password, args.country)
        code = input("6-digit MFA code from email: ").strip()
        client.login(email, password, args.country, mfa_code=code)

    device = client.get_device(args.device_id)
    local_key = device.get("localKey")
    if not local_key:
        raise SystemExit(f"no localKey in response; keys present: {sorted(device)}")

    out = pathlib.Path(args.out)
    out.write_text(local_key)
    out.chmod(0o600)
    print(f"wrote {len(local_key)} chars to {out} (name: {device.get('name')!r})")


if __name__ == "__main__":
    os.umask(0o077)
    main()
