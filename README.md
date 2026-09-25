# Handover Google Messages adapter

This adapter is the Google Messages process for Handover. It is not a
standalone Messages client. Account session and phone relay stay here.
Handover only gets conversations, messages, statuses, typing, and read
updates.

## Status

Google may change the Messages for Web companion protocol without notice. The
adapter pins upstream `mautrix-gmessages` and reports unsupported operations
instead of pretending they work.

## Install

Tagged GitHub Releases attach a Linux amd64 tarball. Unpack it and run
`./install.sh`, then:

```sh
export HANDOVER_GMESSAGES_HELPER=$HOME/.local/bin/handover-gmessages
```

Or build from source with Go:

```sh
git clone https://github.com/mananlalwani/handover-gmessages.git
cd handover-gmessages
go build -o handover-gmessages .
export HANDOVER_GMESSAGES_HELPER=$PWD/handover-gmessages
```

Handover starts the adapter. Do not put account credentials on the command
line.

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

- [Handover](https://github.com/mananlalwani/handover)
- [User guide](docs/user-guide.md)
- [Pairing runbook](docs/pairing-runbook.md)
- [Handover helper contract](https://github.com/mananlalwani/handover/blob/main/docs/gmessages-sidecar.md)
- [License](LICENSE)
