# Contributing to the Google Messages adapter

This repository contains the separate AGPL-3.0-only Google Messages adapter for
Handover. Handover itself is maintained in a different MIT-licensed repository.

## Before changing code

Read:

- [`README.md`](README.md) for setup and repository boundaries.
- [`docs/user-guide.md`](docs/user-guide.md) for account operations.
- [`docs/pairing-runbook.md`](docs/pairing-runbook.md) for login and recovery.
- Handover's [helper contract](https://github.com/mananlalwani/handover/blob/main/docs/gmessages-sidecar.md)
  before changing IPC records.

Keep Google protocol code and account secrets inside this repository. Do not
copy the adapter or its upstream library into Handover.

## Development setup

Use the Go version declared in `go.mod`:

```sh
go build ./...
```

For a local Handover run, build the binary and set:

```sh
export HANDOVER_GMESSAGES_HELPER=$PWD/handover-gmessages-adapter
```

Use a test account and a conversation whose participants have agreed to the
test. Never automate messages to an uninvolved recipient.

## Verification

Run these checks before submitting a change:

```sh
gofmt -w .
go vet ./...
go test ./...
```

Test changes to pairing, persistence, synchronization, status mapping, or
recovery with the relevant unit tests and a safe live account when possible.
Record live results without committing account identifiers, login data, message
text, media, tokens, keys, or logs.

## Contract rules

- Keep helper records coarse and backend-independent.
- Use opaque identifiers at the Handover boundary.
- Keep credentials, cookies, tokens, keys, and message bodies out of logs.
- Report accepted sends separately from delivered, displayed, or failed sends.
- Do not claim a capability or delivery state that the upstream connection did
  not confirm.
- Bound queues, retries, frame sizes, and stored history.
- Preserve restart recovery and offline state transitions.

Changes to the helper contract must be coordinated with the Handover
repository. Update both sides' tests and documentation when a record changes.

## Commits and pull requests

Keep commits focused. Explain behavior that could not be tested. Do not commit
generated binaries, credentials, local session state, private test data, or
live logs.
