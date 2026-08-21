// Package config loads and validates hccast YAML.
package config

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// File is the on-disk document.
type File struct {
	Config Config             `yaml:"config"`
	PerIP  map[string]PerIP   `yaml:"per_ip"`
}

// Config holds daemon-wide defaults.
type Config struct {
	RouterID        string        `yaml:"router_id"`
	AS              uint32        `yaml:"as"`
	ListenPort      int32         `yaml:"listen_port"`
	GracefulRestart bool          `yaml:"graceful_restart"`
	Origin          string        `yaml:"origin"` // igp|egp|incomplete
	Nexthop         string        `yaml:"nexthop"` // self | <ip>
	LocalPref       *uint32       `yaml:"local_pref"`
	MED             *uint32       `yaml:"med"`
	ASPathPrepend   []uint32      `yaml:"as_path_prepend"`
	Communities     []string      `yaml:"communities"`
	Health          Health        `yaml:"health"`
	RouteSelect     RouteSelect   `yaml:"route_select"`
	ReconcileInterval time.Duration `yaml:"reconcile_interval"`
	Peers           []Peer        `yaml:"peers"`
}

// Health rules for iface alias.
type Health struct {
	RequireToken  string `yaml:"require_token"`
	MaxAgeSeconds int    `yaml:"max_age_seconds"`
}

// RouteSelect filters which FIB routes are eligible.
// OnLinkOnly / RejectDefaults default true when omitted.
type RouteSelect struct {
	OnLinkOnly     *bool `yaml:"on_link_only"`
	RejectDefaults *bool `yaml:"reject_defaults"`
	Tables         []int `yaml:"tables"` // empty = any
}

func (r RouteSelect) OnLink() bool {
	if r.OnLinkOnly == nil {
		return true
	}
	return *r.OnLinkOnly
}

func (r RouteSelect) SkipDefaults() bool {
	if r.RejectDefaults == nil {
		return true
	}
	return *r.RejectDefaults
}

// Peer is a BGP neighbor.
type Peer struct {
	IP string `yaml:"ip"`
	AS uint32 `yaml:"as"`
}

// PerIP overrides for one prefix. Pointer/nil means "inherit global".
// Communities/ASPathPrepend: if non-nil slice (including empty), replace global.
type PerIP struct {
	Communities   *[]string `yaml:"communities"`
	ASPathPrepend *[]uint32 `yaml:"as_path_prepend"`
	MED           *uint32   `yaml:"med"`
	LocalPref     *uint32   `yaml:"local_pref"`
	Announce      *bool     `yaml:"announce"`
	Origin        *string   `yaml:"origin"`
	Nexthop       *string   `yaml:"nexthop"`
}

// PathAttrs is the resolved BGP appearance for one prefix.
type PathAttrs struct {
	Origin        uint8 // 0=igp 1=egp 2=incomplete
	Nexthop       string // "self" or IP
	LocalPref     *uint32
	MED           *uint32
	ASPathPrepend []uint32
	Communities   []uint32 // already parsed
	Announce      bool
}

// Load reads and validates a YAML config file.
func Load(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse validates YAML bytes.
func Parse(b []byte) (*File, error) {
	var f File
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	applyDefaults(&f.Config)
	if err := validate(&f); err != nil {
		return nil, err
	}
	return &f, nil
}

func applyDefaults(c *Config) {
	if c.Origin == "" {
		c.Origin = "igp"
	}
	if c.Nexthop == "" {
		c.Nexthop = "self"
	}
	if c.Health.RequireToken == "" {
		c.Health.RequireToken = "healthcheck:ok"
	}
	if c.Health.MaxAgeSeconds == 0 {
		// omitted / zero → 300; alias without date stays forever either way
		c.Health.MaxAgeSeconds = 300
	}
	if c.ReconcileInterval == 0 {
		c.ReconcileInterval = 30 * time.Second
	}
	if c.ListenPort == 0 {
		c.ListenPort = -1
	}
}

func validate(f *File) error {
	c := &f.Config
	if c.GracefulRestart {
		return fmt.Errorf("graceful_restart: true is refused in v1 (health semantics require GR off)")
	}
	if net.ParseIP(c.RouterID) == nil || net.ParseIP(c.RouterID).To4() == nil {
		return fmt.Errorf("router_id: must be IPv4 address")
	}
	if c.AS == 0 {
		return fmt.Errorf("as: must be non-zero")
	}
	if _, err := OriginCode(c.Origin); err != nil {
		return err
	}
	if c.Nexthop != "self" {
		if ip := net.ParseIP(c.Nexthop); ip == nil {
			return fmt.Errorf("nexthop: must be 'self' or an IP")
		}
	}
	if c.ReconcileInterval <= 0 {
		return fmt.Errorf("reconcile_interval: must be > 0")
	}
	if c.Health.MaxAgeSeconds < 0 {
		return fmt.Errorf("health.max_age_seconds: must be >= 0")
	}
	if len(c.Peers) == 0 {
		return fmt.Errorf("peers: at least one peer required")
	}
	for i, p := range c.Peers {
		if net.ParseIP(p.IP) == nil {
			return fmt.Errorf("peers[%d].ip: invalid", i)
		}
		if p.AS == 0 {
			return fmt.Errorf("peers[%d].as: must be non-zero", i)
		}
	}
	for _, s := range c.Communities {
		if _, err := ParseCommunity(s); err != nil {
			return fmt.Errorf("communities: %w", err)
		}
	}
	for pfx, ov := range f.PerIP {
		if _, err := netip.ParsePrefix(pfx); err != nil {
			return fmt.Errorf("per_ip[%s]: invalid prefix: %w", pfx, err)
		}
		if ov.Communities != nil {
			for _, s := range *ov.Communities {
				if _, err := ParseCommunity(s); err != nil {
					return fmt.Errorf("per_ip[%s].communities: %w", pfx, err)
				}
			}
		}
		if ov.Origin != nil {
			if _, err := OriginCode(*ov.Origin); err != nil {
				return fmt.Errorf("per_ip[%s]: %w", pfx, err)
			}
		}
		if ov.Nexthop != nil && *ov.Nexthop != "self" {
			if net.ParseIP(*ov.Nexthop) == nil {
				return fmt.Errorf("per_ip[%s].nexthop: must be 'self' or an IP", pfx)
			}
		}
	}
	return nil
}

// OriginCode maps origin name to BGP code.
func OriginCode(s string) (uint8, error) {
	switch strings.ToLower(s) {
	case "igp":
		return 0, nil
	case "egp":
		return 1, nil
	case "incomplete":
		return 2, nil
	default:
		return 0, fmt.Errorf("origin: must be igp|egp|incomplete")
	}
}

// ParseCommunity parses "ASN:VAL", decimal, or well-known name.
func ParseCommunity(s string) (uint32, error) {
	if n, err := strconv.ParseUint(s, 10, 32); err == nil {
		return uint32(n), nil
	}
	if a, b, ok := strings.Cut(s, ":"); ok {
		hi, err1 := strconv.ParseUint(a, 10, 16)
		lo, err2 := strconv.ParseUint(b, 10, 16)
		if err1 == nil && err2 == nil {
			return uint32(hi)<<16 | uint32(lo), nil
		}
	}
	switch strings.ToLower(s) {
	case "no-export":
		return 0xffffff01, nil
	case "no-advertise":
		return 0xffffff02, nil
	case "no-export-subconfed":
		return 0xffffff03, nil
	case "blackhole":
		return 0xffff029a, nil
	}
	return 0, fmt.Errorf("invalid community %q", s)
}

// Resolve returns PathAttrs for prefix, merging per_ip over globals.
func (f *File) Resolve(prefix string) PathAttrs {
	c := f.Config
	origin, _ := OriginCode(c.Origin)
	attrs := PathAttrs{
		Origin:        origin,
		Nexthop:       c.Nexthop,
		LocalPref:     c.LocalPref,
		MED:           c.MED,
		ASPathPrepend: append([]uint32(nil), c.ASPathPrepend...),
		Announce:      true,
	}
	attrs.Communities = mustParseCommunities(c.Communities)

	ov, ok := f.PerIP[prefix]
	if !ok {
		return attrs
	}
	if ov.Announce != nil {
		attrs.Announce = *ov.Announce
	}
	if ov.Communities != nil {
		attrs.Communities = mustParseCommunities(*ov.Communities)
	}
	if ov.ASPathPrepend != nil {
		attrs.ASPathPrepend = append([]uint32(nil), (*ov.ASPathPrepend)...)
	}
	if ov.MED != nil {
		attrs.MED = ov.MED
	}
	if ov.LocalPref != nil {
		attrs.LocalPref = ov.LocalPref
	}
	if ov.Origin != nil {
		attrs.Origin, _ = OriginCode(*ov.Origin)
	}
	if ov.Nexthop != nil {
		attrs.Nexthop = *ov.Nexthop
	}
	return attrs
}

func mustParseCommunities(ss []string) []uint32 {
	out := make([]uint32, 0, len(ss))
	for _, s := range ss {
		n, err := ParseCommunity(s)
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

// Equal reports whether two PathAttrs are identical for advertise purposes.
// slices.Equal treats nil and empty as equal (unlike reflect.DeepEqual).
func (a PathAttrs) Equal(b PathAttrs) bool {
	return a.Origin == b.Origin && a.Nexthop == b.Nexthop && a.Announce == b.Announce &&
		ptrEq(a.LocalPref, b.LocalPref) && ptrEq(a.MED, b.MED) &&
		slices.Equal(a.ASPathPrepend, b.ASPathPrepend) && slices.Equal(a.Communities, b.Communities)
}

func ptrEq(a, b *uint32) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
