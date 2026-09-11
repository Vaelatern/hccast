package reconcile

import (
	"net/netip"
	"testing"
	"time"

	"github.com/Vaelatern/hccast/internal/config"
)

func baseCfg(t *testing.T) *config.File {
	t.Helper()
	y := `
config:
  router_id: 10.0.255.254
  as: 65000
  communities: ["65000:1100"]
  health:
    require_token: "healthcheck:ok"
    max_age_seconds: 300
  route_select:
    on_link_only: true
    reject_defaults: true
  peers:
    - ip: 10.0.1.1
      as: 65000
per_ip:
  10.0.0.9/32:
    announce: false
  10.0.0.2/32:
    communities: ["65000:100"]
`
	f, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestDesired(t *testing.T) {
	now := time.Unix(1_710_000_000, 0)
	cfg := baseCfg(t)
	pfx1 := netip.MustParsePrefix("10.0.0.1/32")
	pfx2 := netip.MustParsePrefix("10.0.0.2/32")
	pfx9 := netip.MustParsePrefix("10.0.0.9/32")
	def4 := netip.MustParsePrefix("0.0.0.0/0")
	via := netip.MustParseAddr("10.0.0.254")

	linksOK := []Link{{Index: 1, Alias: "healthcheck:ok", Up: true}}
	routeOK := []Route{{Dst: pfx1, LinkIndex: 1}}

	cases := []struct {
		name   string
		links  []Link
		routes []Route
		want   []string // prefixes present
		attrs  string   // optional: check this prefix's community
	}{
		{"healthy+route", linksOK, routeOK, []string{"10.0.0.1/32"}, ""},
		{"stale date", []Link{{Index: 1, Alias: "healthcheck:ok;date:1709990000", Up: true}}, routeOK, nil, ""},
		{"link down", []Link{{Index: 1, Alias: "healthcheck:ok", Up: false}}, routeOK, nil, ""},
		{"default skipped", linksOK, []Route{{Dst: def4, LinkIndex: 1}}, nil, ""},
		{"announce false", linksOK, []Route{{Dst: pfx9, LinkIndex: 1}}, nil, ""},
		{"per_ip attrs", linksOK, []Route{{Dst: pfx2, LinkIndex: 1}}, []string{"10.0.0.2/32"}, "10.0.0.2/32"},
		{"has via skipped", linksOK, []Route{{Dst: pfx1, LinkIndex: 1, Gw: via}}, nil, ""},
		{"proto kernel skipped", linksOK, []Route{{Dst: pfx1, LinkIndex: 1, Protocol: 2}}, nil, ""},
		{"link-local v4 skipped", linksOK, []Route{{Dst: netip.MustParsePrefix("169.254.0.0/16"), LinkIndex: 1}}, nil, ""},
		{"link-local v6 skipped", linksOK, []Route{{Dst: netip.MustParsePrefix("fe80::/64"), LinkIndex: 1}}, nil, ""},
		{"missing token", []Link{{Index: 1, Alias: "", Up: true}}, routeOK, nil, ""},
		{"wrong oif", linksOK, []Route{{Dst: pfx1, LinkIndex: 99}}, nil, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Desired(tc.links, tc.routes, cfg, now)
			if len(got) != len(tc.want) {
				t.Fatalf("len=%d want %v got %v", len(got), tc.want, keys(got))
			}
			for _, p := range tc.want {
				if _, ok := got[p]; !ok {
					t.Fatalf("missing %s in %v", p, keys(got))
				}
			}
			if tc.attrs != "" {
				a := got[tc.attrs]
				if len(a.Communities) != 1 || a.Communities[0] != (65000<<16|100) {
					t.Fatalf("attrs %v", a.Communities)
				}
			}
		})
	}
}

func keys(m map[string]config.PathAttrs) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
