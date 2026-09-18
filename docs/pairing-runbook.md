# Google Messages adapter pairing runbook

This runbook requires the phone and Google account owner. Use a private browser
window and a self/consenting conversation for development. Never automate sends
to a real recipient.

## Login and pair

1. In a private browser window for one Google account, open:

   `https://accounts.google.com/AccountChooser?continue=https://messages.google.com/web/config`
2. Reload the page, copy the `/web/config` request as cURL from developer tools,
   and save it to a local file. Do not paste the request into a shell command.
   Restrict the file while it exists:

   ```sh
   umask 077
   curl_file="$(mktemp)"
   trap 'rm -f "$curl_file"' EXIT
   "${EDITOR:-vi}" "$curl_file"
   ```

   Paste the browser's "Copy as cURL" output into the editor, save, and exit.
   The temporary file has mode 0600 because of `umask 077`.

3. Convert that local file to the adapter's JSON envelope and send it through
   stdin. The cookie values stay out of the command line and shell history:

   ```sh
   python3 contrib/curl-to-envelope.py "$curl_file" |
     handoverctl messages login gmessages:personal
   ```

   The same command accepts a file containing only the `Cookie` header value.
   The adapter envelope is UTF-8 JSON with this shape. Replace the values in a
   local file or let the converter create it; never put real values in a shell
   command, issue, or document:

   ```json
   {"cookies":{"SID":"<value>","HSID":"<value>","SSID":"<value>","OSID":"<value>","APISID":"<value>","SAPISID":"<value>"}}
   ```

   `SID`, `HSID`, `SSID`, `OSID`, `APISID`, and `SAPISID` are the minimum
   required cookies. The converter preserves every cookie it captures because
   Google may require additional cookies for a particular account or session.
   It validates the six minimum names, and the adapter stores the resulting
   session under `${XDG_STATE_HOME:-~/.local/state}/handover/gmessages-adapter`.
   Remove any source file immediately after login. The bundle must never appear
   in argv, shell history, logs, or a committed file.
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
