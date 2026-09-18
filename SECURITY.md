# Security policy

## Reporting a vulnerability

Do not report security issues in a public GitHub issue. Use GitHub's private
vulnerability reporting for this repository. Include a short description, the
affected revision, reproduction steps that do not contain secrets, and the
impact. If private reporting is unavailable, contact the maintainers through a
private channel before sharing details.

Never include Google cookies, tokens, credential bundles, pairing data, private
message contents, media, or sensitive logs in a public issue, pull request, or
discussion. Redact account identifiers and phone numbers as well.

## Scope

The adapter keeps Google authentication and relay protocol data in this
repository and exposes only the normalized Handover helper contract to the
separate MIT repository. Reports about credential handling, session storage,
pairing, relay traffic, or secret-bearing logs are in scope.

The adapter is licensed under AGPL-3.0-only. Do not send private Google data or
upstream protocol material with a report.
