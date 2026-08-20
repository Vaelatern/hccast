# PROMPT — Build hccast

You are implementing **hccast**, a Linux Go program that embeds GoBGP and
advertises (or withdraws) BGP routes based on local on-link FIB entries and
per-interface health aliases.

Read and obey:

- `EXPLANATION.md` — design rationale (do not contradict it).
- `USAGE.md` — external contracts (alias format, config, peers, SIGHUP).

Deliver a working module/repo that matches those documents.

---

## Goal

Build `hccast` so that:

1. GoBGP runs **in-process** (library mode), not as an external `gobgpd`.
2. **Graceful Restart is off** by default (process death ⇒ BGP session death ⇒
   peers flush routes).
3. hccast **never installs** kernel routes (advertise-only; no zebra FIB write).
4. Eligibility for a prefix:
   - unicast route exists, **on-link** (`dev <iface>`, no `via`), oif = iface I;
   - not `0.0.0.0/0` or `::/0`;
   - iface I is oper UP;
   - iface alias parses and satisfies health rules;
   - `per_ip[prefix].announce` is not `false`.
5. Desired set is computed **statelessly** from links + routes + config + now;
   in-memory `have` is updated by diffing AddPath/DeletePath against GoBGP.
6. Inputs: netlink link+route subscribe, reconcile timer, SIGHUP config reload.

---

## Repository layout (suggested)

```text
hccast/
  cmd/hccast/main.go
  internal/config/     # load, validate, merge per_ip
  internal/alias/      # parse healthcheck alias
  internal/netwatch/   # link/route subscribe, dumps, watch health
  internal/reconcile/  # desired set + diff against advertised
  internal/bgp/        # GoBGP server wrapper (peers, AddPath, DeletePath)
  EXPLANATION.md       # already provided — keep/copy into repo
  USAGE.md             # already provided
  PROMPT.md            # this file
  go.mod
  README.md            # short pointer to USAGE.md
```

Use Go modules. Prefer:

- `github.com/osrg/gobgp/v4` (or current stable v4 API) as library;
- `github.com/vishvananda/netlink` for links/routes/alias;
- YAML config (`gopkg.in/yaml.v3` or similar).

Target Linux. Use build tags if needed; do not pretend to support non-Linux
netlink.

---

## Config schema

Implement approximately:

```yaml
config:
  router_id: 10.0.255.254
  as: 65000
  listen_port: -1
  graceful_restart: false

  origin: igp                 # igp | egp | incomplete
  nexthop: self               # self | <ip>
  local_pref: 100
  med: null
  as_path_prepend: []

  communities: []             # strings like "65000:1100" or well-known names if you support them

  health:
    require_token: "healthcheck:ok"
    max_age_seconds: 300

  route_select:
    on_link_only: true
    reject_defaults: true
    # tables: [254]           # optional

  reconcile_interval: 30s

  peers:
    - ip: 10.0.1.1
      as: 65000

per_ip:
  10.0.0.1/32:
    communities: ["65000:1100", "65000:100"]
    as_path_prepend: []
    med: null
    local_pref: null
    announce: true
```

Requirements:

- Validate on load: router_id, as, peers, interval > 0, max_age >= 0, prefixes.
- Invalid SIGHUP reload → log error, **keep previous config**.
- `per_ip` overrides use **replace** semantics for overridden fields (especially
  communities).
- Expose `reconcile_interval` as a real knob.
- Do **not** add a first-class `netlink_recv_buffer` config key; optional
  internal constant / hidden flag only.
- `graceful_restart` must default false; if someone sets true, either refuse or
  implement carefully — prefer refuse/warn in v1 and keep GR off.

CLI (minimal):

```text
hccast -f /etc/hccast.yaml [-v]
```

---

## Alias parser

Input: iface alias string.

- Split on `;`, then each field on first `:`.
- Require token equal to `config.health.require_token` (default `healthcheck:ok`).
- Optional `date=<unix seconds>`: invalid if `now - date > max_age_seconds`.
- Missing `date` ⇒ always fresh.
- Ignore `signed` and unknown keys.
- Unit-test parser thoroughly (ok, missing token, stale date, no date, junk).

---

## Netlink watch

- On start: `LinkList` + route dump (IPv4 and IPv6 as supported).
- `LinkSubscribeWithOptions` and `RouteSubscribeWithOptions` with
  `ErrorCallback` and preferably `ListExisting` as appropriate.
- Treat as **watch died**: subscribe error, repeated ErrorCallback failures,
  or update channel closed → set unhealthy, backoff resubscribe, rely on timer
  full dumps meanwhile.
- Set a sensible SO_RCVBUF default in code (e.g. 1 MiB, non-force).
- Debounce bursts (e.g. 50ms) into a single reconcile.

Timer: always fire every `reconcile_interval` to:

- expire stale `date:` aliases;
- heal missed netlink events via full dump.

---

## Reconcile algorithm

```text
desired = map[prefix]PathAttrs
for each link:
  if !UP: continue
  if !aliasOK(link.Alias, now): continue
  for each route with oif == link.Index:
    if reject(route): continue   # defaults, non-onlink, wrong table, etc.
    attrs = merge(config, per_ip[prefix])
    if attrs.announce == false: continue
    desired[prefix] = attrs

for prefix in desired not in have: AddPath(...)
for prefix in have not in desired: DeletePath(...)
for prefix in both where attrs changed: replace path (delete+add or update)
have = desired
```

Path construction for GoBGP:

- NLRI = prefix;
- next-hop per config (`self` ⇒ appropriate local address policy / GoBGP
  nexthop self behavior);
- origin, communities, AS_PATH prepending, local pref / MED when set;
- identical attrs for all prefixes unless `per_ip` says otherwise.

Idempotent: safe to call reconcile repeatedly.

---

## BGP wrapper

- `server.NewBgpServer` + `Serve` in a goroutine.
- `StartBgp` with ASN, router-id, listen port from config.
- Add/remove/update peers on config apply without resetting unchanged peers.
- Advertise-only: do not enable zebra or otherwise install into kernel FIB.
- No GR capability negotiation in v1.

---

## SIGHUP

- Notify main loop via channel.
- Reload file → validate → atomic swap → peer diff → reconcile.
- Do not cancel netlink watches or destroy `BgpServer` on reload.

Also handle SIGTERM/SIGINT for clean process exit (BGP sockets close with
process; optional polite Notification is fine but not required for v1).

---

## Logging & observability

- Structured logs: peer state, reconcile adds/withdraws, alias reject reasons
  (stale date, missing token), watch healthy/unhealthy, config reload ok/fail.
- Optional metrics later; not required for v1.
- Verbose flag for debug netlink events.

---

## Tests

Must include:

1. Alias parser unit tests.
2. Config load/merge/`per_ip` replace semantics.
3. Desired-set pure function tests given fake links/routes/config/now
   (table-driven): healthy+route⇒advertise; stale date⇒empty; link down;
   default route skipped; `announce: false`; attrs from per_ip.
4. If feasible, netlink/BGP integration tests behind build tags or manual
   script documented in README — not blocking v1 if unit tests cover logic.

---

## Acceptance checklist

- [ ] `hccast -f config.yaml` peers up with embedded GoBGP.
- [ ] On-link route + `healthcheck:ok` alias ⇒ path advertised.
- [ ] Alias cleared / not ok / stale `date` ⇒ path withdrawn.
- [ ] Route deleted or link down ⇒ withdrawn.
- [ ] Defaults not advertised.
- [ ] Killing hccast drops BGP session (GR not enabled).
- [ ] SIGHUP reloads communities/peers/health without unnecessary session reset.
- [ ] Timer withdraws after date expiry without netlink alias change.
- [ ] EXPLANATION.md / USAGE.md remain accurate; update them only if you
      discover a necessary deviation and document it.

---

## Out of scope for v1

- Signify / `signed:` verification (parse/ignore only).
- Writing routes or managing interfaces.
- Fancy CLI route introspection (optional nice-to-have).
- LLGR / GR helper/restarting speaker modes.
- Public `netlink_recv_buffer` config key.

---

## Implementation order

1. Config + alias parser + pure `desired` computation + tests.
2. Netwatch dumps/subscribe + reconcile loop (log desired only).
3. GoBGP wrapper + AddPath/DeletePath wiring.
4. SIGHUP + peer diff.
5. Polish logging, README, manual test notes.

Build the smallest correct program that satisfies USAGE.md contracts and
preserves the failure model in EXPLANATION.md.
