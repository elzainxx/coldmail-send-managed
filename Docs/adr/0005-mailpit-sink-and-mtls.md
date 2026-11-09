# ADR-0005: Mailpit SMTP sink (OD-5) + mTLS box channel

- **Status:** accepted
- **Date:** 2026-07-22

## Context
The real-infra harness needs a genuine SMTP session (STARTTLS-before-AUTH, implicit TLS) so
the box exercises the real transport-security path, not a mock. The box↔Brain channel must be
mutual-TLS with the registration token as the authorization authority.

## Decision (RULED: Mailpit)
Use `axllent/mailpit` as the SMTP sink: real SMTP session with configurable STARTTLS/AUTH and
implicit TLS, a stable query API, and high-throughput ingestion for the E11 load suite.
The box↔Brain channel is mTLS (`RequireAndVerifyClientCert` on the Brain); the token is
verified against its bcrypt hash on register, independent of the cert (token=authority,
cert=transport, P9-A).

## Consequences
Phase 0 does not yet run an SMTP send (no send path until Phase 5), so Mailpit in Phase 0
proves the container comes up and is reachable; the send path plugs into it from Phase 3/5.
The mTLS handshake IS exercised in Phase 0 by box register/heartbeat.

## Links
CLAUDE.md Rule 6; `Docs/phase-0-groundwork.md` §4.3, §6.2; open-decisions.md OD-5.
