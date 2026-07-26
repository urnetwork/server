# Running the egress prober under systemd

The egress prober measures each provider's real exit location by opening a tunnel
**through that provider** and asking public geolocation APIs what address they see.
It must never be able to reach those APIs any other way. A direct lookup would
record the *operator's own* location for the provider and hand the operator's
address to third-party APIs.

A Docker deployment can confine it by attaching the prober to an
`internal: true` network -- no gateway, no NAT -- and to no other network. The
mainstream deployment uses no Docker at all, so this page gives the equivalent:
a systemd unit whose egress is denied by the kernel.

`IPAddressDeny=` / `IPAddressAllow=` are enforced by systemd's cgroup/BPF filter.
No container, no network namespace, no capability grant, and no root — the
filter applies to the service's cgroup regardless of the user it runs as.

## Prerequisites

- systemd 235 or newer, cgroup v2, and BPF available. If any is missing systemd
  logs `Failed to add IP address ... ignoring` and **the address rules do
  nothing**. See [Verifying it is actually enforced](#verifying-it-is-actually-enforced).
- The `egress-prober` binary from
  `https://github.com/Ryanmello07/urnetwork-operator-proxy` at
  `/usr/local/bin/egress-prober`:

  ```bash
  git clone https://github.com/Ryanmello07/urnetwork-operator-proxy.git
  git clone https://github.com/urnetwork/connect.git       # ../connect replace
  git clone https://github.com/urnetwork/glog.git          # ../glog replace
  cd urnetwork-operator-proxy
  CGO_ENABLED=0 go build -trimpath -o /usr/local/bin/egress-prober ./cmd/egress-prober
  ```

## The unit

Save as `/etc/systemd/system/urnetwork-egress-prober.service`.

```ini
[Unit]
Description=URnetwork provider egress prober
Documentation=https://github.com/urnetwork/server/blob/main/docs/operator/prober-systemd.md
After=network-online.target nss-lookup.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/egress-prober \
    --api-url=https://api.example.net \
    --platform-url=wss://connect.example.net \
    --interval=1h \
    --concurrency=4 \
    --probe-timeout=60s \
    --confinement-timeout=3s

# UR_PROBER_BY_JWT and UR_OPERATOR_SECRET. systemd reads this file as PID 1,
# before dropping privileges, so it can be root-owned 0600 even though the
# service itself runs as an unprivileged dynamic user:
#   chown root:root /etc/urnetwork/prober.env && chmod 600 /etc/urnetwork/prober.env
# Passing either secret on the command line would publish it in `ps` and in
# `systemctl show`.
EnvironmentFile=/etc/urnetwork/prober.env

# ---------------------------------------------------------------------------
# THE CONFINEMENT. Everything below this line is the point of the unit.
# ---------------------------------------------------------------------------
# Deny all IP traffic, then allow back exactly the operator's own platform and
# loopback. Enforced by the kernel through systemd's cgroup BPF filter: a
# connection to anything not listed fails with EPERM at connect() time.
IPAddressDeny=any

# Loopback. Required so the process can talk to the local DNS stub resolver
# (systemd-resolved on 127.0.0.53). See "DNS" below -- this is what lets the
# self-check resolve the geolocation hostnames and then discover it cannot
# reach them, which is the evidence it needs to start.
IPAddressAllow=localhost

# The operator's platform. IPAddressAllow TAKES ADDRESSES, NOT HOSTNAMES --
# see "Addresses, not hostnames" below. Replace these with the addresses of
# your own api and connect hosts:
#   getent ahostsv4 api.example.net connect.example.net
IPAddressAllow=198.51.100.10        # api.example.net
IPAddressAllow=198.51.100.11        # connect.example.net
# If your DNS resolver is not on loopback, add its address here too, or use
# --confinement-address (see "DNS" below):
# IPAddressAllow=192.0.2.53

# No privileges of any kind. The provider tunnel is a userspace gvisor
# netstack, not a kernel tun device, so the prober needs no capabilities.
DynamicUser=yes
NoNewPrivileges=yes
CapabilityBoundingSet=
AmbientCapabilities=
RestrictAddressFamilies=AF_INET AF_INET6
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictNamespaces=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallFilter=@system-service
SystemCallArchitectures=native

# A failed confinement self-check is a configuration fault, not a blip. Retry
# a few times in case the platform was briefly unreachable, then stop and stay
# failed so it is visible in `systemctl status` instead of looping forever.
Restart=on-failure
RestartSec=60s
StartLimitIntervalSec=600
StartLimitBurst=5

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now urnetwork-egress-prober
journalctl -u urnetwork-egress-prober -f
```

A healthy start logs:

```
egress-prober: confinement self-check: 3 geolocation host(s) -> 4 address(es): ...
egress-prober: confinement self-check passed: 4 address(es) tested, none directly reachable
```

## Never set `--skip-confinement-check`

The flag exists for a one-shot manual probe from a host you know is not the
operator's. A check disabled in a unit file is not a check: it makes the
process start in exactly the situation the unit is meant to prevent, and the
only trace is two `WARNING` lines in the journal. It is not in the unit above
and it must not be added to it.

## DNS: why loopback is allowed

The prober's self-check refuses to start when it cannot obtain evidence either
way. If it cannot resolve the geolocation hostnames it exits with
`ErrNoEvidence` — being unable to verify is not evidence of confinement. So a
unit with `IPAddressDeny=any` and nothing allowed back does **not** produce a
confined prober; it produces one that will not start.

There are two ways to resolve this, and this unit takes the first:

1. **Permit DNS (this unit).** `IPAddressAllow=localhost` lets the prober reach
   the local stub resolver. `systemd-resolved` is a separate, unrestricted
   service, so it performs the upstream query on the prober's behalf and the
   prober's own egress stays denied. The self-check then resolves
   `ip.pn`, `free.freeipapi.com` and `ipinfo.io`, tries to connect to each
   address, gets `EPERM` from the BPF filter, and starts. That is the healthy
   case, and it needs no hand-maintained list of geolocation addresses.

   If your resolver is *not* on loopback (`/etc/resolv.conf` points at
   `192.0.2.53`, say), add that address to `IPAddressAllow` as well — a DNS
   server address is not a geolocation API, so allowing it does not weaken the
   confinement.

2. **Skip resolution.** Where DNS is genuinely unavailable, pass the addresses
   to dial directly and the check stays real:

   ```ini
   ExecStart=/usr/local/bin/egress-prober \
       ... \
       --confinement-address=134.119.216.174:443 \
       --confinement-address=104.21.94.136:443 \
       --confinement-address=172.67.168.79:443 \
       --confinement-address=34.117.59.81:443
   ```

   The flag is repeatable and rejects hostnames — a name could not be resolved
   at dial time either, so accepting one would mean dialling nothing. This is a
   second copy of the endpoint list and it can go stale; the prober prints the
   hostnames it expects (`geolocation hosts: ...`) on every start so drift is
   visible in the journal. A confined docker deployment has to take this
   route, because an `internal: true` docker network cannot resolve external
   names at all -- the embedded resolver answers SERVFAIL, having no route to
   forward the query.

## Addresses, not hostnames

`IPAddressAllow=` takes IP addresses and CIDR ranges (plus the aliases
`localhost`, `link-local`, `multicast`, `any`). **It does not take hostnames.**
systemd resolves nothing here — there is no name to resolve at packet-filter
time.

That has two consequences an operator has to live with:

- You must list your platform's addresses literally, and **update the unit when
  they change**. A platform address that moves — a new load balancer, a new
  region, a provider migration — breaks the prober silently in the *safe*
  direction: it can no longer reach `api` or `connect`, every pass logs a
  failure, and no probe is submitted. Nothing leaks.
- The tempting fix is to widen the rule: `IPAddressAllow=any` "just to get it
  running", or a whole `/8` because the platform "is somewhere in there".
  **That is the dangerous direction, and the prober's own self-check is what
  catches it.** On the next start the check finds a geolocation address
  directly reachable and refuses to run:

  ```
  egress-prober: confinement self-check failed: confinement: a direct connection
    to a geolocation address succeeded; this process is not confined: 134.119.216.174:443
  egress-prober: this process must not be able to reach a geolocation api except
    through a provider tunnel. ...
  ```

  Exit code 1, and with the `Restart=on-failure` limits above the unit lands in
  `failed`. The same is true if the address rules are not being enforced at all
  (no cgroup v2, no BPF, an old systemd): the prober discovers it can reach the
  APIs and stops. The confinement and the check are two halves of one
  mechanism — the unit restricts, the check proves the restriction is real.

## Verifying it is actually enforced

Confirm systemd accepted the rules, rather than trusting that it did:

```bash
systemctl show urnetwork-egress-prober -p IPAddressDeny -p IPAddressAllow
journalctl -u urnetwork-egress-prober | grep -i 'ip address'   # expect no "ignoring"
```

Then confirm from the outside that the deny actually bites, using the service's
own settings but a harmless command:

```bash
sudo systemd-run --uid=nobody \
  -p IPAddressDeny=any -p IPAddressAllow=localhost \
  --wait --pty /bin/sh -c 'curl -sS --max-time 5 https://ipinfo.io/ip; echo rc=$?'
# expect a connect failure (rc != 0), not an address
```

If that prints an address, the filter is not being applied on this host and the
prober must not be run here until it is.

## Alternative: one pass per timer

`--interval=0` runs a single pass and exits, reporting success or failure in the
exit code, for operators who prefer a timer to a long-running service. Use
`Type=oneshot`, drop `Restart=`, and add a companion
`urnetwork-egress-prober.timer`. Everything in the confinement block above stays
exactly the same — including the self-check, which then runs on every firing.
