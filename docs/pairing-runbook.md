# Google Messages adapter pairing runbook

This runbook requires the phone and Google account owner. Use a private browser
window and a self/consenting conversation for development. Never automate sends
to a real recipient.

## Login and pair

1. In a private browser window for one Google account, open:

   `https://accounts.google.com/AccountChooser?continue=https://messages.google.com/web/config`
2. Reload the page, copy the `/web/config` request from developer tools, and
   create the documented cookie envelope locally.
3. Base64 the envelope without newlines and pipe it to Handover:

   ```sh
   printf '%s' '<base64-envelope>' |
     handoverctl messages login gmessages:personal --from-file /dev/stdin
   ```

   The bundle must never appear in argv, shell history, logs, or a committed
   file.
4. Confirm the matching emoji in Google Messages when prompted.
5. Verify the account and conversations:

   ```sh
   handoverctl messages accounts
   handoverctl messages conversations gmessages:personal
   ```

## Self-test

With a consenting conversation, exercise history, text, media, reply, reaction,
read, and typing operations. Acceptance is distinct from later attested
`sent`, `delivered`, or `displayed` status events.

```sh
handoverctl messages history gmessages:personal:<thread> --limit 20
handoverctl messages send gmessages:personal:<thread> "handover test - ignore"
handoverctl monitor
```

## Recovery and logout

Restart the adapter or `handoverd`; persisted sessions should recover and state
should resynchronize without a new ceremony. During helper loss, the account
must be reported offline and return online only after reconnection.

When remote revoke is intended:

```sh
handoverctl messages logout gmessages:personal
```

Local state is deleted only after remote revoke succeeds. See the Handover-side
contract and security rules in
[`docs/gmessages-sidecar.md`](https://github.com/mananlalwani/handover/blob/main/docs/gmessages-sidecar.md).
