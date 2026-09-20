# Handover Google Messages adapter

This adapter lets Handover use Google Messages from a Linux desktop. It keeps
the Google account session and phone connection in a separate process. Handover
receives conversations, messages, statuses, and typing or read-state updates.

The adapter is an add-on for [Handover](https://github.com/mananlalwani/handover),
not a standalone Google Messages client.

## Status

This project depends on the private companion protocol used by Google Messages
for Web. Google may change that protocol without notice. The adapter pins its
upstream library version and reports unsupported operations instead of claiming
that they work.

## Install

Build the adapter with Go:

```sh
git clone https://github.com/mananlalwani/handover-gmessages.git
cd handover-gmessages
go build -o handover-gmessages .
```

Point Handover at the binary:

```sh
export HANDOVER_GMESSAGES_HELPER=/path/to/handover-gmessages
```

Handover starts and supervises the adapter. You do not need to pass account
credentials in command-line arguments.

## Pair an account

Read the [user guide](docs/user-guide.md) before pairing. It explains account
login, emoji confirmation, testing, recovery, and logout.

## Security and privacy

- Account credentials, cookies, tokens, and keys stay in the adapter process.
- The adapter does not log message text, contacts, media, cookies, tokens, or
  keys.
- The adapter sends Handover normalized records and opaque identifiers, not
  Google protocol objects or account secrets.
- Use a private browser window while obtaining the login data. Never put the
  credential bundle in shell history, command arguments, logs, or Git.

## License boundary

This adapter is licensed under AGPL-3.0-only. Handover is MIT-licensed and lives
in a separate repository. Do not copy this adapter, its upstream Google
protocol library, or generated protocol files into Handover.

## Development

```sh
go vet ./...
go test ./...
```

Read [`CONTRIBUTING.md`](CONTRIBUTING.md) before changing the adapter or its
helper contract.

## Links

- [User guide](docs/user-guide.md)
- [Pairing runbook](docs/pairing-runbook.md)
- [Handover helper contract](https://github.com/mananlalwani/handover/blob/main/docs/gmessages-sidecar.md)
- [License](LICENSE)
