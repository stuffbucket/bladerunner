# HTTPS for the local OIDC provider

**Status:** Proposed / deferred (2026-06-18). Captured while shipping the web
proxy (`internal/webproxy`); not yet scheduled.

## Problem

`br web` signs the user into the Incus web UI by bouncing the browser through a
local OIDC provider. That provider is served over **plain HTTP**:

- `internal/config/config.go` — `OIDCIssuerURL = http://127.0.0.1:<oidc-port>`
- `cmd/bladerunner/web.go` — `providerBase = fmt.Sprintf("http://127.0.0.1:%s", oidcPort)`
- `internal/provision/cloudinit.go` — `incus config set oidc.issuer "http://…"`

The browser's first navigation in the `br web` flow lands on that HTTP origin
(`/authn/consume`), and Incus's `/oidc/login` later redirects the browser to the
issuer's `/authorize` — also HTTP. Every browser flags an `http://` origin as
**"Not Secure"**, even on loopback. There is nothing to trust away: the warning
is about the *scheme*, not an untrusted certificate.

This is cosmetic today — the HTTP origin is a transient redirect hop the browser
passes *through* on the way to the trusted `https://127.0.0.1:<web-port>/ui/`,
and it is loopback-only (never on the wire, reachable only via host loopback and
the guest's vsock-reverse forward). But it undercuts the "clean, no-warning
sign-in" goal the web proxy was built for, so we want it gone.

### Why the web proxy doesn't already cover it

The web proxy (`internal/webproxy`, port `<web-port>`/18444) only fronts the
**Incus** API. The OIDC provider is a separate host service on `<oidc-port>`
(15556), reached by the browser directly (host loopback) and by Incus over a
**raw vsock-reverse TCP forward** (`start.go`, "start the local OIDC provider
before the VM so the vsock-reverse forwarder can dial it"). Because that forward
is a byte-level relay, TLS terminates end-to-end at the host provider — so the
provider *can* be switched to HTTPS without touching the forward.

## Decision

Serve the OIDC provider over **HTTPS, reusing the web proxy's already-trusted
certificate**, and flip the issuer to `https://`. One cert (`webproxy.crt/key`,
SAN already `127.0.0.1` / `::1` / `localhost`) then covers both browser origins
(15556 and 18444), so a single `br web trust` clears every warning.

### Requirements

1. The browser must never reach a plain-HTTP origin during `br web`. After the
   change, both `/authn/*` (provider) and `/ui/`, `/oidc/*` (proxy→Incus) are
   `https://` on a trusted cert.
2. No second trust step. The provider and the proxy present the *same* leaf
   certificate, so the existing `br web trust` covers both.
3. Incus must still validate the issuer. With an `https://` issuer + a
   self-signed cert, Incus's OIDC relying-party (Go) rejects the discovery/JWKS
   fetch unless the guest trusts the cert — so the cert must be installed in the
   **guest** CA store.
4. No regression to the cold-boot / warm-resume paths or the vsock-reverse
   forward.

## Implementation outline (~5–6 files)

1. **Generate the shared cert early.** Today `webproxy.New` lazily generates
   `webproxy.crt/key` in `VMDir`. Hoist generation so the PEM exists *before*
   both (a) the OIDC provider starts and (b) cloud-init renders — so the same
   cert can be handed to the provider and embedded into the guest CA injection.
2. **Provider → `ServeTLS`.** `internal/oidc/provider.go` `Start()` loads the
   cert/key (new fields on `ProviderConfig`) and serves TLS instead of plain
   `Serve`.
3. **Flip issuer strings to `https://`:** `config.go` `OIDCIssuerURL`,
   `web.go` `providerBase`, and the `iss`/discovery value (must all agree — the
   `iss` claim has to match what both the browser and Incus see).
4. **Guest trust (the boot-path part).** In `cloudinit.go`: write the cert to
   the guest CA store (`/usr/local/share/ca-certificates/bladerunner-oidc.crt`)
   and run `update-ca-certificates` **before** Incus reads `oidc.issuer`, then
   `incus config set oidc.issuer "https://…"`.

## Risks

- **Boot/provisioning path.** Step 4 modifies cloud-init — the area that caused
  the warm-boot stage races earlier in this work. `update-ca-certificates` must
  complete before Incus first resolves the issuer, or token exchange fails.
- **Cert timing.** The cert must be generated before cloud-init renders; today
  it is generated lazily at proxy start. Reordering needs care so a failure
  can't strand boot.
- **Issuer-string consistency.** The `iss` claim, `oidc.issuer`, discovery
  document, and `br web`'s `providerBase` must all switch together; a mismatch
  silently breaks SSO (`iss` validation).

Steps 1–3 are low-risk and host-side; step 4 is the only one that warrants
caution and staged verification.

## Alternatives considered

- **Fold the provider behind the web proxy** (single browser origin, 18444).
  Rejected as more code, not less: the `iss` claim must be one value seen by
  both browser and Incus, so the issuer would become `https://…:18444`, the
  proxy would need path-based routing for the provider's browser-facing
  endpoints (`/authn/*`, `/authorize`), *and* the guest would need an 18444
  forward plus the same CA trust. More moving parts for the same guest-trust
  requirement.
- **Leave it as HTTP (current state).** Zero risk; the warning is a transient
  loopback-only hop. This is the chosen state until the work is scheduled.
