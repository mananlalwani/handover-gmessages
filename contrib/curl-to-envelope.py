#!/usr/bin/env python3
"""Build a Handover login envelope from a saved `curl` request.

Usage:
    python3 curl-to-envelope.py /tmp/gm/curl.txt |
        handoverctl messages login gmessages:personal

Reads a request copied from browser devtools ("Copy as cURL"), extracts
the Cookie header, and prints {"cookies": {...}} JSON to stdout. The
envelope never touches disk; pipe it straight into handoverctl, which
base64-encodes it from stdin.

Only cookie names are ever reported in errors, never values. Do not
paste the curl file or this output into chats, issues, or logs.
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
    if not match:
        print(
            "no Cookie header found: re-copy the /web/config request "
            "as cURL (POSIX/bash)",
            file=sys.stderr,
        )
        return 1
    jar = {}
    for piece in match.group(1).strip().rstrip("\\").split(";"):
        if "=" in piece:
            key, value = piece.strip().split("=", 1)
            jar[key.strip()] = value.strip()
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
