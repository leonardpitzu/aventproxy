#!/usr/bin/env python3
"""Work out how the monitor frames its media messages.

`avent-webrtc-bridge lan --dump-media f.bin` writes every decrypted media
message, headers included, as a little-endian u32 length followed by the
message. This reads that back and answers the question the Go code currently
guesses at: is one message one payload, or does a picture span several?

    python3 tools/media_frame_analyze.py /tmp/media.bin [--write-annexb out.h264]

The reassembly it tries is the obvious one: messages sharing a timestamp belong
to the same access unit. If the resulting Annex-B stream decodes, the framing
is settled and the Go side can follow it.
"""

from __future__ import annotations

import argparse
import collections
import struct
import sys
from pathlib import Path

CMD_VIDEO = 0x00010003
CMD_AUDIO = 0x00010005
HEADER_LEN = 32
SUB_HEADER_LEN = 16
SUB_HEADER_OVERHEAD = 12  # the length field counts the timestamp and flags too

NAL_NAMES = {1: "slice", 5: "IDR", 6: "SEI", 7: "SPS", 8: "PPS", 28: "FU-A"}


def read_messages(path: Path):
    """Yield each raw message from the dump."""
    data = path.read_bytes()
    offset = 0
    while offset + 4 <= len(data):
        (size,) = struct.unpack_from("<I", data, offset)
        offset += 4
        if size == 0 or offset + size > len(data):
            break
        yield data[offset : offset + size]
        offset += size


def parse(msg: bytes) -> dict | None:
    """Split one message the way pkg/lan does."""
    if len(msg) < HEADER_LEN + SUB_HEADER_LEN:
        return None
    (cmd,) = struct.unpack_from("<I", msg, 0)
    sub = msg[HEADER_LEN:]
    declared, timestamp, flags = struct.unpack_from("<IQI", sub, 0)
    payload = sub[SUB_HEADER_LEN:]
    return {
        "cmd": cmd,
        "declared": declared,
        "data_len": declared - SUB_HEADER_OVERHEAD,
        "timestamp": timestamp,
        "flags": flags,
        "payload": payload,
        "desc": msg[24:32],
    }


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("dump", type=Path)
    ap.add_argument("--write-annexb", type=Path, help="reassemble and write an Annex-B stream to check with ffmpeg")
    args = ap.parse_args()

    video = [m for m in map(parse, read_messages(args.dump)) if m and m["cmd"] == CMD_VIDEO]
    audio = [m for m in map(parse, read_messages(args.dump)) if m and m["cmd"] == CMD_AUDIO]
    if not video:
        print("no video messages in the dump")
        return 1

    print(f"video messages: {len(video)}   audio messages: {len(audio)}")

    # Does one message hold its whole unit, or is the declared length bigger?
    complete = sum(1 for m in video if m["data_len"] <= len(m["payload"]))
    print(f"\ndeclared length fits inside the message: {complete}/{len(video)}")
    if complete != len(video):
        short = [m for m in video if m["data_len"] > len(m["payload"])]
        print(
            f"  {len(short)} messages declare more than they carry, e.g. "
            f"declared {short[0]['data_len']} vs carried {len(short[0]['payload'])}"
        )
        print("  => the declared length spans several messages: a unit is continued")

    # Messages sharing a timestamp should be one picture.
    groups = collections.OrderedDict()
    for m in video:
        groups.setdefault(m["timestamp"], []).append(m)
    sizes = collections.Counter(len(g) for g in groups.values())
    print(f"\naccess units by shared timestamp: {len(groups)}")
    print(f"  messages per unit: {dict(sorted(sizes.items()))}")

    # What does the first byte of each message look like, first in its unit or not?
    first_types = collections.Counter()
    rest_types = collections.Counter()
    for msgs in groups.values():
        for i, m in enumerate(msgs):
            if not m["payload"]:
                continue
            t = m["payload"][0] & 0x1F
            (first_types if i == 0 else rest_types).update([t])
    print("\nfirst byte & 0x1f, message that opens a unit:")
    for t, n in first_types.most_common(6):
        print(f"  {t:2d} {NAL_NAMES.get(t, ''):6s} {n}")
    print("continuation messages:")
    for t, n in rest_types.most_common(6):
        print(f"  {t:2d} {NAL_NAMES.get(t, ''):6s} {n}")
    spread = len(rest_types)
    print(
        f"  distinct values: {spread} "
        f"({'uniform, so these are raw continuation bytes' if spread > 20 else 'structured'})"
    )

    # Flags may mark the start or end of a unit.
    print("\nflags by position in unit:")
    for label, idx in (("first", 0), ("last", -1)):
        vals = collections.Counter(msgs[idx]["flags"] for msgs in groups.values() if msgs)
        print(f"  {label}: {dict(vals.most_common(4))}")

    print("\ndescription bytes (first unit):", video[0]["desc"].hex())

    if args.write_annexb:
        # A unit is its messages' payloads in order; the research says the data
        # is raw NALs with no start codes, so one start code per unit.
        out = bytearray()
        for msgs in groups.values():
            out += b"\x00\x00\x00\x01" + b"".join(m["payload"] for m in msgs)
        args.write_annexb.write_bytes(out)
        print(f"\nwrote {len(out)} bytes to {args.write_annexb}")
        print(f"check it with:  ffprobe -v error -show_streams {args.write_annexb}")
        print(f"                ffmpeg -i {args.write_annexb} -f null -")

    return 0


if __name__ == "__main__":
    sys.exit(main())
