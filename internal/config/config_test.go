package config

import (
	"testing"
	"time"
)

func TestParseMinimal(t *testing.T) {
	y := `
config:
  router_id: 10.0.255.254
  as: 65000
  listen_port: -1
  graceful_restart: false
  origin: igp
  nexthop: self
  local_pref: 100
  communities: ["65000:1100"]
  health:
    require_token: "healthcheck:ok"
    max_age_seconds: 300
  route_select:
    on_link_only: true
    reject_defaults: true
  reconcile_interval: 30s
  peers:
    - ip: 10.0.1.1
      as: 65000
`
	f, err := Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if f.Config.AS != 65000 || f.Config.ReconcileInterval != 30*time.Second {
		t.Fatalf("%+v", f.Config)
	}
	c, err := ParseCommunity("65000:1100")
	if err != nil || c != (65000<<16|1100) {
		t.Fatalf("community %v %v", c, err)
	}
}

func TestGracefulRestartRefused(t *testing.T) {
	y := `
config:
  router_id: 10.0.255.254
  as: 65000
  graceful_restart: true
  peers:
    - ip: 10.0.1.1
      as: 65000
`
	if _, err := Parse([]byte(y)); err == nil {
		t.Fatal("expected GR refuse")
	}
}

func TestPerIPReplaceSemantics(t *testing.T) {
	y := `
config:
  router_id: 10.0.255.254
  as: 65000
  communities: ["65000:1100"]
  as_path_prepend: [65000]
  local_pref: 100
  peers:
    - ip: 10.0.1.1
      as: 65000
per_ip:
  10.0.0.1/32:
    communities: ["65000:100"]
    as_path_prepend: []
    announce: false
  10.0.0.2/32:
    local_pref: 200
`
	f, err := Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	a := f.Resolve("10.0.0.1/32")
	if a.Announce {
		t.Fatal("announce should be false")
	}
	if len(a.Communities) != 1 || a.Communities[0] != (65000<<16|100) {
		t.Fatalf("communities replaced, got %v", a.Communities)
	}
	if len(a.ASPathPrepend) != 0 {
		t.Fatalf("prepend replaced with empty, got %v", a.ASPathPrepend)
	}
	if a.LocalPref == nil || *a.LocalPref != 100 {
		t.Fatal("local_pref should inherit")
	}

	b := f.Resolve("10.0.0.2/32")
	if len(b.Communities) != 1 || b.Communities[0] != (65000<<16|1100) {
		t.Fatalf("inherit communities, got %v", b.Communities)
	}
	if b.LocalPref == nil || *b.LocalPref != 200 {
		t.Fatalf("local_pref override got %v", b.LocalPref)
	}
	if !b.Announce {
		t.Fatal("announce default true")
	}

	c := f.Resolve("10.0.0.3/32")
	if !c.Equal(f.Resolve("10.0.0.3/32")) {
		t.Fatal("equal")
	}
	if len(c.ASPathPrepend) != 1 || c.ASPathPrepend[0] != 65000 {
		t.Fatalf("global prepend %v", c.ASPathPrepend)
	}
}

func TestInvalidPeer(t *testing.T) {
	y := `
config:
  router_id: 10.0.255.254
  as: 65000
  peers: []
`
	if _, err := Parse([]byte(y)); err == nil {
		t.Fatal("expected error")
	}
}
