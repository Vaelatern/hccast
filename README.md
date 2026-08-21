# hccast

Health-check cast: advertise on-link Linux FIB routes over embedded GoBGP when
the egress interface carries a valid health alias.

```bash
go build -o hccast ./cmd/hccast
hccast -f /etc/hccast.yaml
```

See **USAGE.md** for alias format, config, peers, and SIGHUP.
See **EXPLANATION.md** for why GR is off and how desired-set reconcile works.

## Manual smoke test

```bash
# terminal A
sudo ./hccast -f ./hccast.example.yaml -v

# terminal B — intent + health
sudo ip link add vethhc0 type dummy
sudo ip link set vethhc0 up
sudo ip route add 203.0.113.50/32 dev vethhc0
sudo ip link set vethhc0 alias "healthcheck:ok;date:$(date +%s)"
# peer should see 203.0.113.50/32

sudo ip link set vethhc0 alias ""
# → withdraw
```

## Tests

```bash
go test ./...
```
