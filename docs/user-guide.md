# Google Messages adapter user guide

The adapter connects Handover to one Google Messages account. Handover starts
the adapter and exposes its conversations through `handoverctl`.

## Before you start

You need:

- Handover installed and running on Linux.
- A phone with Google Messages signed in.
- The adapter binary built from this repository.
- Access to the Google account that owns the phone.

The phone must stay online with background data enabled. This adapter uses the
Google Messages for Web companion connection. It does not use a public Google
Messages API.

## Configure Handover

Point Handover at the adapter binary:

```sh
export HANDOVER_GMESSAGES_HELPER=/path/to/handover-gmessages
```

If Handover runs as a systemd user service, add the variable to the service's
environment using your normal user-service configuration. Do not put account
credentials in the service file or in command-line arguments.

Restart Handover after changing the setting:

```sh
systemctl --user restart handoverd
```

## Sign in and pair

Follow the [pairing runbook](pairing-runbook.md). In short, you will copy the
login request data from Google Messages for Web, pass it to Handover through
standard input, and confirm the matching emoji on the phone.

The login data is sensitive. Do not save it in a file, shell history, command
argument, log, or Git commit.

After pairing, check the account and its conversations:

```sh
handoverctl messages accounts
handoverctl messages conversations gmessages:personal
```

## Send and inspect messages

Use a conversation owned by you or a person who agreed to the test. Do not send
automated messages to an unconsenting recipient.

```sh
handoverctl messages history gmessages:personal:<thread> --limit 20
handoverctl messages send gmessages:personal:<thread> "handover test - ignore"
handoverctl monitor
```

An accepted send is not proof that Google delivered or displayed the message.
Wait for an explicit status before treating the send as complete.

## Restart and recover

Restarting Handover or the adapter should reuse the saved session and
resynchronize account state. A disconnected account must appear offline until
the helper reconnects.

Sessions persist as 0600 files below
`${XDG_STATE_HOME:-~/.local/state}/handover/gmessages`. Staged
attachments are transient transfer data, swept at adapter startup:
files older than 7 days go, and the directory is capped at 256 MiB
oldest-first.

If the session cannot recover, repeat the pairing process. Check the service
logs without sharing their contents publicly:

```sh
journalctl --user -u handoverd --since today
```

## Logout

Revoke the remote session and remove the local session through Handover:

```sh
handoverctl messages logout gmessages:personal
```

Pair again if you want to reconnect the account later.

## What the adapter supports

The adapter can relay text and supported media. Reactions, replies, read
receipts, and typing depend on the conversation type and on what Google attests
for that conversation. Unsupported operations are reported as unsupported.

The adapter does not invent delivery states, capabilities, or progress.
