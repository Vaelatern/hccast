package bgp

import (
	"testing"

	"github.com/Vaelatern/hccast/internal/config"
	api "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	apipacket "github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

func TestAddDeletePath(t *testing.T) {
	y := `
config:
  router_id: 10.0.255.254
  as: 65000
  listen_port: -1
  peers:
    - ip: 203.0.113.1
      as: 65001
`
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	lp := uint32(100)
	attrs := config.PathAttrs{
		Origin:      0,
		Nexthop:     "self",
		LocalPref:   &lp,
		Communities: []uint32{65000<<16 | 1100},
		Announce:    true,
	}
	if err := srv.AddPath("203.0.113.50/32", attrs); err != nil {
		t.Fatalf("AddPath: %v", err)
	}
	if err := srv.AddPath("2001:db8::50/128", attrs); err != nil {
		t.Fatalf("AddPath v6: %v", err)
	}

	var n int
	for _, fam := range []apipacket.Family{apipacket.RF_IPv4_UC, apipacket.RF_IPv6_UC} {
		err := srv.s.ListPath(apiutil.ListPathRequest{
			TableType: api.TableType_TABLE_TYPE_GLOBAL,
			Family:    fam,
		}, func(prefix apipacket.NLRI, paths []*apiutil.Path) {
			n += len(paths)
		})
		if err != nil {
			t.Fatalf("ListPath %v: %v", fam, err)
		}
	}
	if n < 2 {
		t.Fatalf("expected >=2 paths in global rib, got %d", n)
	}

	if err := srv.DeletePath("203.0.113.50/32"); err != nil {
		t.Fatalf("DeletePath: %v", err)
	}
	if err := srv.DeletePath("2001:db8::50/128"); err != nil {
		t.Fatalf("DeletePath v6: %v", err)
	}
}
