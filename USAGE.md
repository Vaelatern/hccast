# hccast — Usage

What the world around hccast must provide, how to configure it, and how
advertisements behave at runtime.

## Role boundaries

| Actor | Responsibility |
|-------|----------------|
| **Someone else** | Create interfaces, run health probes, set iface alias, install on-link routes |
| **hccast** | Watch links/routes, validate alias, advertise/withdraw via BGP |
| **BGP peer** | Receive announcements; apply its own policy |

hccast never installs FIB routes. If the kernel has no on-link route, hccast
has nothing to announce for that prefix.

## Prerequisites

- Linux (netlink + `IFLA_IFALIAS`).
- Permission to create BGP sockets (typically `CAP_NET_BIND_SERVICE` if
  listening on 179, and always whatever your environment needs for outbound
  BGP). Reading link/route netlink generally does not need `CAP_NET_ADMIN`;
  setting aliases does (done by the prober, not necessarily by hccast).
- A BGP peer reachable at a configured IP/ASN.
- GoBGP embedded in-process (no separate `gobgpd` required).

## Health signal: interface alias

Probers mark an interface healthy by setting its alias:

```bash
sudo ip link set dev vethh-adfa8380 alias "healthcheck:ok"
```

With a freshness timestamp (unix seconds):

```bash
sudo ip link set dev vethh-adfa8380 alias "healthcheck:ok;date:$(date +%s)"
```

Reserved future form (ignored until verification is implemented):

```text
healthcheck:ok;signed:DEADBEEF;date:1710000000
```

### Alias rules hccast enforces

- Fields are `;`-separated `key:value` tokens.
- **`healthcheck:ok` must be present** or the iface is ineligible.
- **`date`**: if present, alias is invalid when older than
  `config.health.max_age_seconds` (default `300`). If absent, date is
  considered always valid.
- **`signed`**: ignored in v1.
- Unknown keys: ignored.
- Max length: 255 printable characters (kernel `IFALIASZ`).

Inspect:

```bash
cat /sys/class/net/vethh-adfa8380/ifalias
ip -d link show vethh-adfa8380
```

Clear:

```bash
sudo ip link set dev vethh-adfa8380 alias ""
```

## Intent routes: on-link in the FIB

Install the prefix you want announced so it points at the healthchecked iface:

```bash
sudo ip route add 10.0.0.1/32 dev vethh-adfa8380
# IPv6
sudo ip -6 route add 2001:db8::1/128 dev vethh-adfa8380
```

Expectations:

- **On-link / `dev`-only** routes (no `via`), unless you later extend policy.
- When the iface alias is valid **and** the link is UP **and** the route
  exists, hccast advertises that NLRI to configured peers.
- Removing the route, moving it off the iface, clearing/invalidating the
  alias, or taking the link down causes **withdraw**.

Defaults / wide aggregates pointed at the iface should not be advertised
(hccast must skip `0.0.0.0/0` and `::/0`).

## Configuration file

Default path: implementation-defined (e.g. `./hccast.yaml` or
`/etc/hccast.yaml`). YAML recommended.

### Minimal example (normal case)

```yaml
config:
  router_id: 10.0.255.254
  as: 65000
  listen_port: -1          # no listen; outbound/active peer only (or set 179)
  graceful_restart: false  # required health semantics

  origin: igp
  nexthop: self
  local_pref: 100
  # med: null
  as_path_prepend: []

  communities:
    - "65000:1100"         # your site’s “from hccast” tag; optional

  health:
    require_token: "healthcheck:ok"
    max_age_seconds: 300

  route_select:
    on_link_only: true
    # tables: [254]        # main table; optional filter
    reject_defaults: true

  reconcile_interval: 30s

  peers:
    - ip: 10.0.1.1
      as: 65000
```

Ship **only** `config:` unless you need exceptions.

### Optional per-prefix overrides

Use when two prefixes on the same box must look different to upstream policy:

```yaml
per_ip:
  10.0.0.1/32:
    communities:
      - "65000:1100"
      - "65000:100"      # e.g. gold / voice class
    # as_path_prepend: [65000]
    # med: 50
    # local_pref: 200
    # announce: false    # never advertise even if healthy
```

Merge: `per_ip` **replaces** overriding fields from `config` for that prefix
(including the communities list). Prefixes not listed use globals unchanged.

### What operators usually set

| Field | Why |
|-------|-----|
| `router_id`, `as` | BGP identity |
| `peers[].ip`, `peers[].as` | Who to speak to |
| `graceful_restart: false` | Death → peer flush |
| `health.*` | Alias token + max age |
| `reconcile_interval` | Date expiry + safety resync cadence |
| `communities` | Optional single origin tag for RR match |

Leave `per_ip` empty until a peer’s route-map needs distinct labels.

## Runtime behavior

### Startup

1. Load config (fail hard if invalid).
2. Start embedded GoBGP; add peers.
3. Dump links + routes; compute `desired`; advertise.
4. Subscribe to netlink link + route updates.
5. Start reconcile timer.

### Ongoing

```text
desired = prefixes with:
  - unicast route, on-link, oif = iface I
  - not default route
  - iface I UP
  - alias parses and satisfies health rules
  - per_ip.announce != false

advertise  desired - have
withdraw   have - desired
```

BGP path attributes come from `config` (and `per_ip` if present): origin,
communities, prepend, nexthop policy, etc. Same namespace / same class ⇒ same
attrs unless overridden.

### SIGHUP

```bash
kill -HUP $(pidof hccast)
```

- Reloads config without tearing down the process.
- Invalid file → log error, **keep old config**.
- Peer list is diffed; unchanged sessions stay up.
- Reconcile runs under the new policy (communities / health / per_ip changes
  apply on the next diff).

### Process exit

- Crash / kill → BGP TCP sessions die → peers withdraw hccast routes
  (no GR).
- Prefer this over leaving stale health advertisements in the network.

## Peer expectations

- Peer address and ASN must match `config.peers`.
- Peer should accept the AFI/SAFIs you originate (IPv4/IPv6 unicast as
  implemented).
- Peer-side communities/route-maps are **their** business; hccast only attaches
  what you configure.
- Hold-timer behavior on silent host death is standard BGP; hccast cannot
  notify if the machine disappears.

## Example end-to-end

```bash
# 1. Intent + healthy alias (done by your orchestration/prober)
sudo ip link set dev vethh-adfa8380 up
sudo ip route add 203.0.113.50/32 dev vethh-adfa8380
sudo ip link set dev vethh-adfa8380 alias "healthcheck:ok;date:$(date +%s)"

# 2. Run hccast with config pointing at your RR/peer
hccast -f /etc/hccast.yaml

# 3. Peer should now see 203.0.113.50/32 from hccast’s ASN

# 4. Fail health
sudo ip link set dev vethh-adfa8380 alias "healthcheck:fail"
# or: sudo ip link set dev vethh-adfa8380 alias ""
# → hccast withdraws

# 5. Or remove intent
sudo ip route del 203.0.113.50/32 dev vethh-adfa8380
# → hccast withdraws
```

## Non-expectations

- Do not expect hccast to create veths, run HTTP probes, or write routes.
- Do not expect GR to preserve routes across hccast restarts.
- Do not put large signatures or prefix lists in the alias field.
- Do not point defaults at HC ifaces and expect them to be cast.

## See also

- `EXPLANATION.md` — why these contracts exist.
- `PROMPT.md` — agent brief to implement the program.
