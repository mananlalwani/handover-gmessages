# handover-gmessages-adapter

A separate Google Messages relay for the Handover messaging subsystem.
It speaks the Handover helper IPC v1 contract on stdin/stdout and drives
the real Google Messages companion service through upstream
[`mautrix/gmessages`/`pkg/libgm`](https://github.com/mautrix/gmessages)
(v0.2609.0, pinned in `go.mod`).

## License boundary

This component is **AGPL-3.0-only** (see `LICENSE`). It links upstream
`libgm` and must stay out of the MIT Handover repository: no file from
here is ever copied there, and no Handover source is vendored here. The
two sides share only the coarse JSON contract (documented below), which
carries opaque ids and normalized records — no Google types, protobuf,
cookies, tokens, or keys cross the process boundary. The Handover side
of the boundary is specified in `docs/gmessages-sidecar.md` over in the
Handover tree; this README specifies the relay side.

## What it does

* One process hosts any number of Google accounts (keyed by the
  contract's `account` field; Handover normally spawns one helper per
  identity and passes the binary path, never secrets, via
  `HANDOVER_GMESSAGES_HELPER` or `PATH`).
* Owns the Gaia cookie login, the UKEY2 emoji pairing ceremony, Tachyon
  token refresh, long-poll recovery, relay RPCs, and media
  upload/download. Sessions persist as 0600 JSON files below
  `${XDG_STATE_HOME:-~/.local/state}/handover/gmessages-adapter`.
* Maps relay threads/messages/statuses/typing/reads into v1 wire
  records. Statuses map 1:1 from the relay taxonomy
  (`accepted/sent/delivered/displayed/failed:<reason>`); drafts,
  schedules, tombstones, and undecryptable markers without content
  produce no signal rather than invented state.
* Capabilities attested: text+media everywhere; reactions, replies,
  read receipts, and typing on RCS threads only (SMS stays text-only);
  own-device delete; attachment download; group open-or-create. There
  are no edit, membership-change, or disappearing-message operations
  because upstream offers none.
* Never logs bodies, cookies, tokens, keys, contacts, or media bytes.
  Logs (stderr) carry ids, counts, and delivery states only.

## Build and check

```sh
go build -o handover-gmessages-adapter .
go vet ./...
go test ./...
```

`go test` covers the status taxonomy exhaustively against the real
upstream enum (any new upstream status fails the build until it is
classified), the exact v1 JSON shapes (required arrays are always
present), secret-store permissions, cursor rules, and filename safety.

## Wiring it to Handover

```sh
export HANDOVER_GMESSAGES_HELPER=/path/to/handover-gmessages-adapter
# start handoverd; it supervises this binary automatically
```

No configuration files, no flags, no argv secrets. `HANDOVER_ADAPTER_DEBUG=1`
enables debug logging (still redacted).

## Pairing runbook (needs the phone + Google account owner)

Handover never sees this material; the bundle below travels
file/stdin → local socket → local helper pipe only.

1. In a **private** browser window with a **single** Google account
   (no Device-Bound Session Credentials), open
   `https://accounts.google.com/AccountChooser?continue=https://messages.google.com/web/config`
   and sign in. Do not navigate elsewhere in that window.
2. In devtools → Network, reload, copy the `/web/config` request, and
   extract the cookies into this envelope (values stay on your machine):
   `{"cookies": {"SID": "...", "HSID": "...", "SSID": "...", "OSID": "...",
   "APISID": "...", "SAPISID": "..."}}`
   (`__Secure-1PSIDTS` is sometimes also required; add it when present).
3. Base64 the envelope **without newlines** and log in (example account
   name `gmessages:personal`):
   ```sh
   printf '%s' '<base64-envelope>' | handoverctl messages login gmessages:personal --from-file /dev/stdin
   ```
4. The helper emits a pairing prompt (one emoji). In Google Messages on
   the phone, confirm the matching emoji when it appears.
5. `handoverctl messages accounts` should show the account online;
   `handoverctl messages conversations <account>` lists threads.

## PoC checklist (self/consenting conversation)

1. Pair as above.
2. `messages history <account>:<thread> --limit 20` shows existing RCS
   history; repeat with `--cursor <token>` for older pages.
3. `messages send <account>:<thread> "handover poc <date> — ignore"`:
   accepted at once, `sent/delivered/displayed` arrive as attested.
4. Watch `handoverctl monitor` for the inbound echo and status events.
5. `messages send-file …` both directions; attachments land staged.
6. Reply (send into the same thread referencing the target), react and
   unreact, `messages typing`, `messages read`.
7. Restart adapter/daemon: sessions restore from disk, state resyncs,
   no new ceremony.
8. `messages logout <account>`: remote revoke runs first; local state
   is deleted only after the revoke succeeds, and the account
   disappears from Handover.

## Known limits

* The phone must stay online with background data; offline phones stall
  history, events, and sends (surfaced as errors, never cached lies).
* Google can and does change the undocumented companion protocol; pin
  the `libgm` version and re-run `go test` (the taxonomy test catches
  new statuses) after upgrades.
* Group read state stays conversation-level: the relay's human-readable
  "Read by …" text is never parsed into per-participant truth.
* Group creation carries no name (the v1 contract has no name field);
  nameless-group rejection surfaces as a failure.
* An unclean daemon kill can orphan this process; the next supervisor
  generation replaces it. Graceful daemon shutdown stops it (verified
  via PPID-tracked lifecycle test against `handoverd`).
* SMS reactions/captions are partial upstream and stay unattested.
