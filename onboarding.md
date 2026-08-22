# Coldstore co-owner onboarding

This runbook gives one additional co-owner full Coldstore access through Tailscale while
exposing no other service on the server. It is a preparation document, not authorization to
change the production host. Host configuration, service changes and credential rotation
require the owner's separate approval.

## Access contract

```text
co-owner browser
    -> TCP 443 inside Tailscale only
    -> Tailscale Serve HTTPS
    -> http://127.0.0.1:8090
    -> Coldstore Basic Auth
    -> existing full owner web UI/API
```

The intended boundary is exact:

- Tailscale Serve, not Funnel;
- one shared server machine, not general membership in the owner's tailnet;
- only TCP 443 on that shared machine;
- only the Coldstore reverse proxy on Serve's HTTPS listener;
- Coldstore remains bound to loopback at `127.0.0.1:8090`;
- Basic Auth remains required after Tailscale admits the connection;
- no SSH, Syncthing UI/API, raw Coldstore port 8090, filesystem share, shell or other server
  service is part of this onboarding; and
- after authentication, the co-owner receives the existing full Coldstore web UI/API,
  including mutating operations. There is no reduced remote role in v1.

Two co-owners share one existing Basic Auth credential. Basic Auth cannot attribute actions
to one person or revoke only one person. That is an accepted v1 limitation; separate
Tailscale identities still allow the network share to be revoked independently.

## Never put these in the repository

Keep all real values in the password manager or Tailscale admin console:

- Basic Auth username or password;
- Tailscale account email/login;
- machine-share invitation URL;
- real `.ts.net` hostname, tailnet name, Tailscale IP or device identifier; and
- host-specific service or policy output.

Do not paste the Basic Auth password into a shell command, URL, issue, pull request or chat.
Share it with the co-owner using the password manager's secure-sharing feature.

## Roles

### Existing owner

- owns or administers the server's Tailscale account;
- reviews the current Serve configuration and tailnet access policy;
- configures the Coldstore-only HTTPS proxy;
- shares only the server machine with the second co-owner;
- shares the existing Basic Auth credential through the password manager; and
- can revoke the machine share and rotate the shared credential.

### Second co-owner

- uses a separate Tailscale account;
- installs Tailscale on each approved personal device;
- accepts the server-machine share;
- reaches Coldstore only through the private HTTPS URL; and
- treats the shared Basic Auth credential as a full control-plane credential.

## Phase 1 — preflight on the server

**Completed from owner-provided output on 2026-08-21:** Tailscale 1.102.3 is installed;
`tailscale serve status` and `tailscale funnel status` both report no configuration; and an
unauthenticated HEAD request to the loopback Coldstore listener returns `401 Unauthorized`
with `WWW-Authenticate: Basic realm="coldstore"`. No private host or tailnet identifiers are
recorded here.

Run these read-only checks and review their output before changing anything:

```bash
tailscale version
tailscale status
tailscale serve status
curl --head http://127.0.0.1:8090/
```

Expected Coldstore result: the loopback request reaches Coldstore and returns an HTTP Basic
Auth challenge (`401 Unauthorized`) when no credential is supplied. A connection failure
means the daemon/listener must be corrected before adding Tailscale Serve. A response that
does not require authentication is a stop condition.

### Stop conditions

Do not continue automatically if any of these is true:

- `tailscale serve status` already shows HTTPS port 443 or another Serve configuration;
- the other Tailscale project uses Serve/Funnel on this server;
- Coldstore is not listening only on the intended loopback address;
- the unauthenticated loopback request does not receive an authentication challenge;
- the tailnet policy contains a broad rule that also grants shared/external users access to
  this machine or its other ports; or
- changing the tailnet policy could affect another project in a way that has not been
  reviewed.

Serve configurations share state. Do not overwrite an existing listener, and do not use
`tailscale serve reset` when another Serve entry might exist: reset clears the full Serve
configuration, not only Coldstore.

## Phase 2 — restrict shared users to Coldstore HTTPS

Tailscale grants are additive. A narrow rule does not cancel a broader matching rule. Review
the entire policy first, including rules whose source is `*`, before adding the grant below.

The identifier-free policy shape for this one-machine sharing model is:

```json
{
  "src": ["autogroup:shared"],
  "dst": ["*"],
  "ip": ["tcp:443"]
}
```

Add that object to the existing `grants` array; do not replace unrelated policy. In this
context, `dst: ["*"]` does not share every machine: an external user can reach only a machine
that was explicitly shared with them, and this grant further limits that reachability to
TCP 443. If the owner later shares a different machine with any external user, reassess this
group-wide rule first.

Use the Tailscale policy editor's validation and preview before saving. If an existing rule
already allows `autogroup:shared`, the co-owner's login, or `*` to reach broader ports, stop
and narrow that rule deliberately. Adding the TCP 443 grant alone will not override it.

## Phase 3 — configure Coldstore-only Serve

After checking that HTTPS 443 is unused by Serve and obtaining separate owner approval, run
the current Tailscale Serve form:

```bash
tailscale serve --bg http://127.0.0.1:8090
```

The command should report a private HTTPS URL ending in `.ts.net` and a single proxy target:

```text
https://SERVER.TAILNET.ts.net/
|-- proxy http://127.0.0.1:8090
```

The shown name is illustrative. Do not copy the real name into this repository.

If Tailscale HTTPS is not enabled for the tailnet, the interactive flow may provide an admin
URL to enable it. Review that page before accepting. The `--bg` flag makes Serve persistent
across a Tailscale restart or server reboot.

Verify the resulting configuration:

```bash
tailscale serve status
```

The status must show only the reviewed Coldstore mapping on HTTPS 443. Do not run
`tailscale funnel`; Funnel makes a service public.

From an already authorized owner device, open the private HTTPS URL without entering a
credential first. Coldstore must present the Basic Auth challenge. Then authenticate using
the password manager and confirm the normal interface loads.

## Phase 4 — share only the server machine

In the Tailscale admin console:

1. Open the server in the Machines page.
2. Choose the machine-sharing action; do not invite the co-owner as a general tailnet member.
3. Create a share for the co-owner's separate Tailscale account.
4. Send the invitation through a private channel.
5. Revoke and recreate the invitation if its link is sent to the wrong person or channel.
6. Wait for the co-owner to accept before sharing the Basic Auth credential.

Sharing the machine does not authorize Coldstore by itself. The co-owner must pass both the
Tailscale boundary and Coldstore Basic Auth.

## Phase 5 — co-owner device setup

On each approved iPhone, macOS, Linux or Windows device:

1. Install the current Tailscale client from the platform's normal trusted source.
2. Sign in with the co-owner's own Tailscale account, not the existing owner's account.
3. Accept the server-machine share.
4. Confirm the shared server appears in Tailscale.
5. Open the exact `.ts.net` HTTPS URL supplied privately by the owner.
6. Retrieve the Coldstore Basic Auth credential from the password-manager share.
7. Authenticate and confirm the normal Coldstore interface loads.

Do not bookmark or use `http://SERVER:8090`, a LAN address, a raw Tailscale IP with port 8090,
or any Funnel URL. The supported path is the private `https://...ts.net/` Serve URL.

## Phase 6 — acceptance checks

Complete these without destructive archive/restore tests:

- [ ] With Tailscale disconnected, the private Coldstore URL is unreachable.
- [ ] With Tailscale connected but no Basic Auth, the URL returns an authentication challenge.
- [ ] With both layers, the existing Coldstore UI and read-only API requests work.
- [ ] From the co-owner's macOS or Linux device, TCP 22 and TCP 8090 on the shared machine are
      unreachable through Tailscale; only TCP 443 is permitted.
- [ ] An ordinary browser can preview an image/document and download a file through HTTPS.
- [ ] An HTTP Range request used by media preview succeeds through Serve.
- [ ] The iPhone works once on Wi-Fi and once with Wi-Fi disabled over cellular.
- [ ] At least one desktop platform works from outside the home LAN when convenient.
- [ ] Reboot or restart evidence confirms the background Serve mapping returns unchanged.
- [ ] `tailscale serve status` still lists only the intended reviewed mapping.

The co-owner is intentionally authorized for mutating operations, but do not archive,
restore, rename, change quota or alter configuration merely to prove access. Exercise those
operations only as part of normal, intentional use.

## Routine use

- Keep Tailscale connected when using Coldstore away from home.
- Use only the private HTTPS Serve URL.
- Let the password manager fill Basic Auth; do not send credentials in URLs or command lines.
- Treat a lost device, leaked password or unexpected share as a security event.
- Periodically review the server-machine share, tailnet policy and `tailscale serve status`.

## Offboarding one co-owner

Because the Tailscale identities are separate but Basic Auth is shared, complete both halves:

1. Revoke the co-owner's machine share in the Tailscale Machines page.
2. Confirm their devices can no longer reach the private server URL.
3. Rotate the Coldstore Basic Auth credential using the production secret-management and
   service-restart procedure.
4. Update the remaining owner's password-manager entry.
5. Confirm the remaining owner can still authenticate.

Revoking only the machine share removes the current network path but leaves the shared Basic
credential known. Rotating only Basic Auth leaves the shared machine reachable. Do both for
complete offboarding.

## Disable or roll back Coldstore Serve

To remove the Coldstore HTTPS listener for everyone while preserving other Serve state:

```bash
tailscale serve --https=443 off
tailscale serve status
```

Do not use `tailscale serve reset` unless a review proves Coldstore is the only configured
Serve endpoint and clearing every Serve entry is intended.

Disabling Serve does not stop Coldstore itself and does not revoke the machine share. Revoke
the share separately when offboarding; rotate Basic Auth if the credential may be exposed.

## References

- [Tailscale Serve command](https://tailscale.com/docs/reference/tailscale-cli/serve)
- [Tailscale Serve overview](https://tailscale.com/docs/features/tailscale-serve)
- [Tailscale machine sharing](https://tailscale.com/docs/features/sharing)
- [Tailscale grants](https://tailscale.com/docs/features/access-control/grants)
- [Tailscale grant syntax](https://tailscale.com/docs/reference/syntax/grants)
