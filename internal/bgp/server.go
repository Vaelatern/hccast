// Package bgp wraps an in-process GoBGP server.
package bgp

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/Vaelatern/hccast/internal/config"
	api "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
)

// Server is the advertise-only GoBGP wrapper.
type Server struct {
	s     *server.BgpServer
	log   *slog.Logger
	mu    sync.Mutex
	peers map[string]config.Peer // keyed by IP
}

// New starts Serve() in a goroutine and StartBgp with cfg.
func New(cfg *config.File, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	lvl := &slog.LevelVar{}
	lvl.Set(slog.LevelInfo)
	s := server.NewBgpServer(server.LoggerOption(log, lvl))
	go s.Serve()

	c := cfg.Config
	if err := s.StartBgp(context.Background(), &api.StartBgpRequest{
		Global: &api.Global{
			Asn:        c.AS,
			RouterId:   c.RouterID,
			ListenPort: c.ListenPort,
		},
	}); err != nil {
		s.Stop()
		return nil, fmt.Errorf("StartBgp: %w", err)
	}
	// No zebra ⇒ we never install peer routes into the FIB. Leave import
	// default-accept so AddPath (local) is not turned into a withdraw.

	srv := &Server{
		s:     s,
		log:   log,
		peers: make(map[string]config.Peer),
	}

	_ = s.WatchEvent(context.Background(), server.WatchEventMessageCallbacks{
		OnPeerUpdate: func(peer *apiutil.WatchEventMessage_PeerEvent, _ time.Time) {
			if peer.Type == apiutil.PEER_EVENT_STATE {
				log.Info("peer state", "peer", peer.Peer)
			}
		},
	})

	if err := srv.ApplyPeers(c.Peers); err != nil {
		s.Stop()
		return nil, err
	}
	return srv, nil
}

// Close stops BGP.
func (s *Server) Close() {
	s.s.Stop() // Stop → StopBgp once, then ancillary servers
}

// ApplyPeers diffs desired peers against current: add/remove. Unchanged stay up.
func (s *Server) ApplyPeers(peers []config.Peer) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	want := make(map[string]config.Peer, len(peers))
	for _, p := range peers {
		want[p.IP] = p
	}
	for ip, old := range s.peers {
		np, ok := want[ip]
		if !ok {
			if err := s.s.DeletePeer(context.Background(), &api.DeletePeerRequest{
				Address: ip,
			}); err != nil {
				s.log.Error("delete peer", "ip", ip, "err", err)
			} else {
				s.log.Info("peer removed", "ip", ip)
			}
			delete(s.peers, ip)
			continue
		}
		if old.AS != np.AS {
			// simplest correct: delete+add
			_ = s.s.DeletePeer(context.Background(), &api.DeletePeerRequest{Address: ip})
			if err := s.addPeer(np); err != nil {
				return err
			}
			s.peers[ip] = np
		}
	}
	for ip, p := range want {
		if _, ok := s.peers[ip]; ok {
			continue
		}
		if err := s.addPeer(p); err != nil {
			return err
		}
		s.peers[ip] = p
		s.log.Info("peer added", "ip", p.IP, "as", p.AS)
	}
	return nil
}

func (s *Server) addPeer(p config.Peer) error {
	peer := &api.Peer{
		Conf: &api.PeerConf{
			NeighborAddress: p.IP,
			PeerAsn:         p.AS,
		},
		AfiSafis: []*api.AfiSafi{
			{Config: &api.AfiSafiConfig{Family: ipv4UC(), Enabled: true}},
			{Config: &api.AfiSafiConfig{Family: ipv6UC(), Enabled: true}},
		},
	}
	return s.s.AddPeer(context.Background(), &api.AddPeerRequest{Peer: peer})
}

func ipv4UC() *api.Family {
	return &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}
}
func ipv6UC() *api.Family {
	return &api.Family{Afi: api.Family_AFI_IP6, Safi: api.Family_SAFI_UNICAST}
}

// AddPath advertises prefix with attrs.
func (s *Server) AddPath(prefix string, attrs config.PathAttrs) error {
	path, err := s.buildPath(prefix, attrs, false)
	if err != nil {
		return err
	}
	_, err = s.s.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{path}})
	return err
}

// DeletePath withdraws prefix.
func (s *Server) DeletePath(prefix string) error {
	path, err := s.buildPath(prefix, config.PathAttrs{Nexthop: "self", Origin: 0}, true)
	if err != nil {
		return err
	}
	return s.s.DeletePath(apiutil.DeletePathRequest{Paths: []*apiutil.Path{path}})
}

func (s *Server) buildPath(prefix string, attrs config.PathAttrs, withdraw bool) (*apiutil.Path, error) {
	pfx, err := netip.ParsePrefix(prefix)
	if err != nil {
		return nil, err
	}
	nlri, err := bgp.NewIPAddrPrefix(pfx)
	if err != nil {
		return nil, err
	}

	family := bgp.RF_IPv4_UC
	if pfx.Addr().Is6() {
		family = bgp.RF_IPv6_UC
	}

	nhStr := attrs.Nexthop
	if nhStr == "" || nhStr == "self" {
		// 0.0.0.0 / :: ⇒ GoBGP rewrites to local address toward each peer.
		if pfx.Addr().Is6() {
			nhStr = "::"
		} else {
			nhStr = "0.0.0.0"
		}
	}
	nh, err := netip.ParseAddr(nhStr)
	if err != nil {
		return nil, fmt.Errorf("nexthop: %w", err)
	}

	var bgpAttrs []bgp.PathAttributeInterface
	bgpAttrs = append(bgpAttrs, bgp.NewPathAttributeOrigin(attrs.Origin))

	// Supply nexthop via NEXT_HOP (v4) or MP_REACH (v6); apiutil2Path rewrites attrs.
	if pfx.Addr().Is4() {
		a, err := bgp.NewPathAttributeNextHop(nh)
		if err != nil {
			return nil, err
		}
		bgpAttrs = append(bgpAttrs, a)
	} else {
		a, err := bgp.NewPathAttributeMpReachNLRI(family, []bgp.PathNLRI{{NLRI: nlri}}, nh)
		if err != nil {
			return nil, err
		}
		bgpAttrs = append(bgpAttrs, a)
	}

	if len(attrs.ASPathPrepend) > 0 {
		bgpAttrs = append(bgpAttrs, bgp.NewPathAttributeAsPath([]bgp.AsPathParamInterface{
			bgp.NewAs4PathParam(bgp.BGP_ASPATH_ATTR_TYPE_SEQ, attrs.ASPathPrepend),
		}))
	}
	if attrs.LocalPref != nil {
		bgpAttrs = append(bgpAttrs, bgp.NewPathAttributeLocalPref(*attrs.LocalPref))
	}
	if attrs.MED != nil {
		bgpAttrs = append(bgpAttrs, bgp.NewPathAttributeMultiExitDisc(*attrs.MED))
	}
	if len(attrs.Communities) > 0 {
		bgpAttrs = append(bgpAttrs, bgp.NewPathAttributeCommunities(attrs.Communities))
	}

	return &apiutil.Path{
		Family:     family,
		Nlri:       nlri,
		Attrs:      bgpAttrs,
		Withdrawal: withdraw,
	}, nil
}

