# Handover Google Messages adapter

This repository contains the production Google Messages relay for Handover. It
speaks Handover helper IPC v1 on stdin/stdout and drives the Google Messages
companion service through upstream
[`mautrix/gmessages`](https://github.com/mautrix/gmessages) `pkg/libgm`, pinned
in `go.mod`.

## Repository boundary

This adapter is **AGPL-3.0-only**; see [`LICENSE`](LICENSE). Handover itself is
MIT-licensed and lives in a separate repository:
[`mananlalwani/handover`](https://github.com/mananlalwani/handover).

Do not copy or vendor this code, upstream `libgm`, Google protocol definitions,
or generated protobufs into Handover. The two repositories share only the
coarse JSON helper contract documented in Handover's
[`docs/gmessages-sidecar.md`](https://github.com/mananlalwani/handover/blob/main/docs/gmessages-sidecar.md).
That boundary carries normalized records and opaque IDs, not Google types,
cookies, tokens, keys, contacts, or message bodies.

## Responsibilities

The adapter owns:

- Google cookie login and UKEY2 emoji pairing.
- Tachyon token refresh and long-poll recovery.
- Conversation/history synchronization and message status transitions.
- Text, reaction, read, typing, and media relay operations supported by the
  upstream protocol.
- Per-account credential persistence below
  `${XDG_STATE_HOME:-~/.local/state}/handover/gmessages-adapter`.

It supports text and media broadly; reactions, replies, read receipts, and
typing are attested only for RCS threads. Unsupported operations are omitted,
not represented as invented capabilities.

The adapter never logs message bodies, cookies, tokens, keys, contacts, or media
bytes. Stderr contains only redacted IDs, counts, and delivery states.

## Build and test

Requires the Go version declared in `go.mod`:

```sh
go build -o handover-gmessages-adapter .
go vet ./...
go test ./...
```

Tests cover the helper contract, status taxonomy, required JSON shapes, secret
store permissions, cursor handling, bounded staging, and filename safety.

## Run with Handover

Build the binary, then point `handoverd` at it:

```sh
export HANDOVER_GMESSAGES_HELPER=/path/to/handover-gmessages-adapter
```

Alternatively place the binary on `PATH` as `handover-gmessages-helper`.
Handover supervises the process, applies bounded restart backoff, marks its
accounts offline when it exits, and resynchronizes them after reconnect. No
configuration files, flags, or secrets in argv are required.

Set `HANDOVER_ADAPTER_DEBUG=1` for additional still-redacted diagnostics.

## Pairing and operations

Follow [`docs/pairing-runbook.md`](docs/pairing-runbook.md) for account login,
emoji confirmation, self/consenting-conversation checks, restart recovery, and
logout. Credentials stay local and are never passed as command-line arguments.

## Limits

- The phone must remain online with background data enabled; failures surface as
  unavailable or failed states rather than cached success.
- Google can change the undocumented companion protocol. Keep the upstream
  version pinned and rerun `go test ./...` after upgrades.
- Group read state remains conversation-level unless the relay attests more
  detail. Group creation has no name field in helper IPC v1.
- An unclean daemon kill may orphan the child temporarily; the next supervisor
  generation replaces it. Graceful daemon shutdown stops it cleanly.
- SMS reactions and captions remain unsupported unless the upstream relay
  attests them.
