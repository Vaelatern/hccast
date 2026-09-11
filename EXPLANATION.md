# hccast — Design Explanation

This document records **why** hccast is shaped the way it is. It is the decision
log for implementers and future agents. Operational expectations live in
`USAGE.md`. The build brief lives in `PROMPT.md`.

## What hccast is

`hccast` (health-check cast) is a small Go program that:

1. Embeds [GoBGP](https://github.com/osrg/gobgp) as an in-process BGP library.
2. Watches the local Linux FIB and link table via netlink.
3. Advertises on-link routes whose egress interface carries a valid health alias.
4. Withdraws those advertisements when health, link, or route state says they
   should no longer be announced.

It does **not** install routes into the kernel. Something else adds
`ip route add <prefix> dev <iface>` intent routes. hccast only mirrors eligible
intent into BGP.

## Core failure model: process death must withdraw

We embed GoBGP in the same process that owns the health→BGP mapping so that:

- if `hccast` dies, its TCP BGP sockets die with it;
- peers see session teardown promptly (FIN/RST while the host is still up);
- without Graceful Restart, peers **flush all routes learned from hccast**.

That is deliberate. For health-check anycast / “attract traffic only while
healthy,” **absence of the speaker is a withdraw signal**.

### Why not Graceful Restart?

Graceful Restart does the opposite of health semantics:

- On crash, helpers **retain stale routes** for `restart-time`.
- Only after the speaker returns, re-advertises, and sends End-of-RIB do extras
  get dropped.

That is excellent for control-plane restarts on a router that still forwards.
It is dangerous for “this process is the health authority.” Traffic would keep
flowing to a dead or restarting checker.

**Default: Graceful Restart off.** Clean death and crashes should remove
routes from the network as soon as the session drops. Planned binary restarts
may flap advertisements; that is accepted.

(Silent host/network failure is different: peers may wait for Hold Timer. That
is normal BGP and out of scope for GR.)

### Why not a long-lived `gobgpd` plus an external injector?

If the injector dies but `gobgpd` keeps the session up, peers keep the routes.
Unless you add leases/heartbeats carefully, you advertise on behalf of a dead
owner. Embedding avoids that class of bug by construction.

## Source of truth split

| Question | Authority |
|----------|-----------|
| Which prefixes *could* be announced? | Kernel FIB: on-link routes `dev <iface>` |
| Is this iface healthy enough to announce? | Interface **alias** (`IFLA_IFALIAS`) |
| How should announcements look in BGP? | hccast config (global + optional per-prefix) |
| What are we currently announcing? | In-memory set maintained by hccast |

The alias is a **flag**, not a database. It enables a **stateless** function:

```text
desired = f(links, routes, config, now)
```

hccast then diffs `desired` against `have` and calls GoBGP `AddPath` /
`DeletePath`. State is only “what we have advertised,” updated from the
stateless reality.

## Why interface alias for health?

Linux `ip link set dev <if> alias "..."` stores up to 255 printable characters
(`IFALIASZ` = 256 including NUL) in `IFLA_IFALIAS`, also visible as
`/sys/class/net/<if>/ifalias`.

We chose alias because:

- it is already bound to the interface that owns the on-link routes;
- netlink `RTM_NEWLINK` notifies on alias changes (no tight sysfs poll for
  content changes);
- other components (probers) can set it with standard `ip` tooling;
- it keeps health signal next to the datapath object without a second registry.

### Alias format

```text
healthcheck:ok;signed:DEADBEEF;date:1710000000
```

Rules:

- `healthcheck:ok` is required for eligibility.
- If `date` (unix seconds) is present, the alias is invalid when
  `now - date > max_age_seconds` (default 300).
- If `date` is absent, the alias is treated as always fresh.
- `signed:` is reserved for later signify-style auth (sign iface name + route
  set or a hash thereof). Ignore until implemented.
- Unknown `key:value` fields separated by `;` must be ignored (forward compatible).

Alias space is tiny; when signatures arrive, prefer signing a hash and storing
a short `signed:<hex>` rather than stuffing full payloads into the alias.

## Why discover routes from the FIB?

Alternative A: config lists every prefix to advertise, then health only gates
them. Simpler reasoning, more drift (config says X, FIB has Y).

Alternative B (chosen): whatever installs on-link routes registers intent in
the kernel; hccast discovers routes whose `oif` is a healthy iface and
advertises those prefixes.

B fits “endpoints appear → route appears → BGP follows” workflows and matches
netlink as the integration surface.

### Seatbelts (even when alias is the primary filter)

- Prefer **explicit on-link** routes (`dev <iface>`, no `via`, or onlink).
- Skip `proto kernel` (connected prefixes from addresses) and link-local
  (`169.254.0.0/16`, `fe80::/10`) — those are not intent.
- Skip default routes (`0.0.0.0/0`, `::/0`) even if oif matches.
- GoBGP advertise-only: **do not** install paths into the FIB (no zebra). That
  avoids feedback loops where we re-advertise what we ourselves wrote.
- Eligibility is still gated on a valid health alias, link UP, and route
  presence.

Retract when any of: alias not ok / date stale, link down, link deleted, or
matching route deleted (or oif moved away).

## BGP attributes: global by default, per-prefix escape hatch

hccast typically announces one **class** of healthchecked routes from one
namespace. Communities, origin, equal AS-path prepend, MED, and next-hop policy
can be set **once** at daemon/peer level (config + GoBGP export policy or attrs
on AddPath).

Per-prefix communities matter only when **downstream** policy must distinguish
services on the same box (QoS class, no-export vs public, blackhole, accounting,
prefer/deprefer). Until that exists, leave `per_ip` empty.

Config shape:

- top-level `config:` defaults for all advertisements;
- optional `per_ip:` overrides (replace semantics for lists like communities —
  less surprising than silent union);
- normal deployments ship **only** `config:`.

There is no universal “correct” community like `65000:2`. Operators pick an
origin tag their RR/edge already matches, or omit communities until asked.

## Watch, don’t poll — but always reconcile on a timer

Primary inputs:

- `LinkSubscribe` — alias changes, operstate, add/delete;
- `RouteSubscribe` — intent route add/del/change.

Alias string changes do **not** require polling `/sys/.../ifalias` when the
netlink watch is healthy; `ip link set ... alias` generates `RTM_NEWLINK`.

A timer is still mandatory because:

1. **`date:` expiry** produces no netlink event;
2. netlink recv buffer overruns / stuck sockets can drop events;
3. periodic full `LinkList` + `RouteList` heals drift.

So the model is hybrid: event-driven reconcile when healthy, plus
`reconcile_interval` (config knob, e.g. 30s) always. On watch death (subscribe
error, `ErrorCallback`, update channel closed), mark watch unhealthy, backoff
resubscribe, and rely on the timer dumps until recovered.

### Config knobs

- **`reconcile_interval`**: yes — operators understand it; ties to health TTL.
- **`netlink_recv_buffer`**: no public knob by default. Use a sensible code
  default (e.g. 1 MiB, non-forcing). Periodic reconcile covers overruns.
  Expose only as a hidden debug flag if production ever needs it.

## SIGHUP reload without interrupting service

SIGHUP must **not** restart GoBGP or drop established peers unnecessarily.

1. Signal handler only notifies the main loop (no heavy work in-handler).
2. Load + validate new config; on failure keep the old config.
3. Atomically swap config.
4. Diff peers: add/remove/update; unchanged peers stay Established.
5. Run the same `reconcile()` as netlink/timer paths.

Netlink subscriptions stay up across reload unless something fundamental (e.g.
netns) changes.

## Library choices

- **GoBGP as library** (`server.NewBgpServer`): in-process sessions, AddPath /
  DeletePath, peer management without a second daemon.
- **`github.com/vishvananda/netlink`**: first-class `Attrs().Alias`,
  `LinkSetAlias`, `LinkSubscribe` / `RouteSubscribe`, route oif — better fit
  than scraping sysfs alone when we already need route watching.

## Non-goals (for the initial program)

- Installing or modifying kernel routes.
- Being a route reflector or full policy engine UI.
- Long-lived Graceful Restart / LLGR for crash tolerance.
- Encoding the full prefix list in the alias.
- Per-route TE unless `per_ip` overrides say so.
- Implementing signify verification in v1 (schema reserved only).

## Summary

hccast treats **kernel on-link routes as intent**, **iface alias as health**,
and **in-process BGP without GR as the withdrawal channel**. Config supplies
daemon-wide BGP appearance with rare per-prefix overrides. Netlink watches
drive freshness; a reconcile timer covers time-based expiry and missed events;
SIGHUP reloads policy without tearing down what we are serving.
