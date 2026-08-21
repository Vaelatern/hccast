package reconcile

import (
	"net/netip"
	"testing"
	"time"

	"github.com/Vaelatern/hccast/internal/config"
)

type memAdv struct {
	added   map[string]config.PathAttrs
	deleted []string
}

func (m *memAdv) AddPath(prefix string, attrs config.PathAttrs) error {
	if m.added == nil {
		m.added = map[string]config.PathAttrs{}
	}
	m.added[prefix] = attrs
	return nil
}
func (m *memAdv) DeletePath(prefix string) error {
	m.deleted = append(m.deleted, prefix)
	delete(m.added, prefix)
	return nil
}

func TestEngineDiff(t *testing.T) {
	cfg := baseCfg(t)
	now := time.Unix(1_710_000_000, 0)
	adv := &memAdv{}
	eng := NewEngine(adv, nil)

	links := []Link{{Index: 1, Alias: "healthcheck:ok", Up: true}}
	routes := []Route{{Dst: netip.MustParsePrefix("10.0.0.1/32"), LinkIndex: 1}}
	eng.Reconcile(links, routes, cfg, now)
	if _, ok := adv.added["10.0.0.1/32"]; !ok {
		t.Fatal("expected advertise")
	}

	eng.Reconcile([]Link{{Index: 1, Alias: "", Up: true}}, routes, cfg, now)
	if len(adv.added) != 0 {
		t.Fatalf("expected withdraw, have %v", adv.added)
	}
	if len(adv.deleted) == 0 {
		t.Fatal("expected DeletePath")
	}

	eng.Reconcile(links, routes, cfg, now)
	routes2 := []Route{
		{Dst: netip.MustParsePrefix("10.0.0.1/32"), LinkIndex: 1},
		{Dst: netip.MustParsePrefix("10.0.0.2/32"), LinkIndex: 1},
	}
	eng.Reconcile(links, routes2, cfg, now)
	if len(adv.added) != 2 {
		t.Fatalf("want 2 got %v", adv.added)
	}
	if adv.added["10.0.0.2/32"].Communities[0] != (65000<<16 | 100) {
		t.Fatalf("per_ip community %v", adv.added["10.0.0.2/32"].Communities)
	}
}
