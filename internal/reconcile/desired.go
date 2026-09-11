// Package reconcile computes the desired advertisement set and diffs it.
package reconcile

import (
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/Vaelatern/hccast/internal/alias"
	"github.com/Vaelatern/hccast/internal/config"
)

// Link is the subset of netlink link state we need.
type Link struct {
	Index int
	Alias string
	Up    bool
}

// RTPROT_KERNEL — connected routes from interface addresses.
const protoKernel = 2

// Route is the subset of netlink route state we need.
type Route struct {
	Dst       netip.Prefix
	LinkIndex int
	Gw        netip.Addr // zero ⇒ on-link / no via
	Table     int
	Protocol  int // RTPROT_*; 2 = kernel
}

// Desired computes the set of prefixes that should be advertised right now.
func Desired(links []Link, routes []Route, cfg *config.File, now time.Time) map[string]config.PathAttrs {
	out := make(map[string]config.PathAttrs)
	if cfg == nil {
		return out
	}
	hc := cfg.Config.Health
	rs := cfg.Config.RouteSelect

	healthy := make(map[int]Link, len(links))
	for _, l := range links {
		if !l.Up {
			continue
		}
		if r := alias.Check(l.Alias, hc.RequireToken, hc.MaxAgeSeconds, now); !r.OK {
			continue
		}
		healthy[l.Index] = l
	}

	for _, rt := range routes {
		if !rt.Dst.IsValid() {
			continue
		}
		if rt.Protocol == protoKernel {
			continue // connected from addresses, not intent
		}
		if rt.Dst.Addr().IsLinkLocalUnicast() {
			continue // fe80::/10, 169.254.0.0/16
		}
		if rs.SkipDefaults() && rt.Dst.Bits() == 0 {
			continue
		}
		if rs.OnLink() && rt.Gw.IsValid() {
			continue // has via ⇒ not on-link
		}
		if len(rs.Tables) > 0 && !slices.Contains(rs.Tables, rt.Table) {
			continue
		}
		if _, ok := healthy[rt.LinkIndex]; !ok {
			continue
		}
		pfx := rt.Dst.String()
		attrs := cfg.Resolve(pfx)
		if !attrs.Announce {
			continue
		}
		out[pfx] = attrs
	}
	return out
}

// IPNetToPrefix converts *net.IPNet to netip.Prefix.
func IPNetToPrefix(n *net.IPNet) (netip.Prefix, bool) {
	if n == nil {
		return netip.Prefix{}, false
	}
	ones, bits := n.Mask.Size()
	if bits == 0 {
		return netip.Prefix{}, false
	}
	ip, ok := netip.AddrFromSlice(n.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(ip, ones), true
}
