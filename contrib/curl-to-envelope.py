#!/usr/bin/env python3
"""Build a Handover login envelope from browser cookie material.

Usage:
    python3 curl-to-envelope.py <cookie-file> |
        handoverctl messages login gmessages:personal

<cookie-file> is either:
  * a request copied from browser devtools ("Copy as cURL"), from which
    the Cookie header is extracted, or
  * a file containing just the raw cookie string (the value of the
    `cookie:` request header, e.g. "SID=...; HSID=...; ...").

The second form avoids devtools copy quirks entirely: in the Network
tab, click the /web/config request, open the Headers pane, and copy the
value of the `cookie` request header into the file.

Prints {"cookies": {...}} JSON to stdout. The envelope never touches
disk; pipe it straight into handoverctl, which base64-encodes it from
stdin.

Only cookie names are ever reported in errors, never values. Do not
paste the cookie file or this output into chats, issues, or logs.
"""

import json
import re
import sys


def main() -> int:
    if len(sys.argv) != 2:
        print("usage: curl-to-envelope.py <curl-file>", file=sys.stderr)
        return 2
    try:
        with open(sys.argv[1], encoding="utf-8", errors="replace") as handle:
            raw = handle.read()
    except OSError as exc:
        print(f"cannot read curl file: {exc}", file=sys.stderr)
        return 1
    match = re.search(r"Cookie:\s*([^\n'\"]+)", raw)
    if match:
        header = match.group(1).strip().rstrip("\\")
    else:
        # No cURL wrapper: treat the whole file as a raw cookie string.
        header = " ".join(raw.split())
    jar = {}
    for piece in header.split(";"):
        if "=" in piece:
            key, value = piece.strip().split("=", 1)
            jar[key.strip()] = value.strip()
    if not jar:
        print(
            "no cookies found: save either a devtools cURL copy or the raw "
            "cookie header value into the file",
            file=sys.stderr,
        )
        return 1
    need = ["SID", "HSID", "SSID", "OSID", "APISID", "SAPISID"]
    missing = [key for key in need if key not in jar]
    if missing:
        print(
            "missing cookies: "
            + ", ".join(missing)
            + " - use a private window with a single Google account "
            "and copy the /web/config request itself",
            file=sys.stderr,
        )
        return 1
    sys.stdout.write(json.dumps({"cookies": jar}))
    return 0


if __name__ == "__main__":
    sys.exit(main())
