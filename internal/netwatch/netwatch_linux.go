//go:build linux

// Package netwatch watches Linux links and routes via netlink.
package netwatch

import (
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Vaelatern/hccast/internal/reconcile"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const recvBuf = 1 << 20 // 1 MiB, non-force

// Watcher maintains link+route state and signals reconcile.
type Watcher struct {
	log       *slog.Logger
	verbose   bool
	mu        sync.Mutex
	links     map[int]reconcile.Link
	routes    map[string]reconcile.Route // key = prefix|oif
	wake      chan struct{}
	healthy   atomic.Bool
	done      chan struct{}
	closeOnce sync.Once
}

func New(log *slog.Logger, verbose bool) *Watcher {
	if log == nil {
		log = slog.Default()
	}
	w := &Watcher{
		log:     log,
		verbose: verbose,
		links:   make(map[int]reconcile.Link),
		routes:  make(map[string]reconcile.Route),
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	w.healthy.Store(true)
	return w
}

// Wake is closed-buffer signal: receive to know something changed.
func (w *Watcher) Wake() <-chan struct{} { return w.wake }

func (w *Watcher) Healthy() bool { return w.healthy.Load() }

func (w *Watcher) Close() {
	w.closeOnce.Do(func() { close(w.done) })
}

func (w *Watcher) kick() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Snapshot returns copies of current link/route maps as slices.
func (w *Watcher) Snapshot() ([]reconcile.Link, []reconcile.Route) {
	w.mu.Lock()
	defer w.mu.Unlock()
	links := make([]reconcile.Link, 0, len(w.links))
	for _, l := range w.links {
		links = append(links, l)
	}
	routes := make([]reconcile.Route, 0, len(w.routes))
	for _, r := range w.routes {
		routes = append(routes, r)
	}
	return links, routes
}

// DumpFull replaces in-memory state from LinkList + RouteList.
func (w *Watcher) DumpFull() error {
	ll, err := netlink.LinkList()
	if err != nil {
		return err
	}
	r4, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	r6, err := netlink.RouteList(nil, netlink.FAMILY_V6)
	if err != nil {
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	w.links = make(map[int]reconcile.Link, len(ll))
	for _, l := range ll {
		w.links[l.Attrs().Index] = linkFrom(l)
	}
	w.routes = make(map[string]reconcile.Route)
	for _, r := range r4 {
		if rr, ok := routeFrom(r); ok {
			w.routes[routeKey(rr)] = rr
		}
	}
	for _, r := range r6 {
		if rr, ok := routeFrom(r); ok {
			w.routes[routeKey(rr)] = rr
		}
	}
	return nil
}

// RunSubscribe loops link+route subscriptions with backoff on death.
func (w *Watcher) RunSubscribe() {
	backoff := time.Second
	for {
		select {
		case <-w.done:
			return
		default:
		}
		err := w.subscribeOnce()
		if err != nil {
			w.log.Warn("netlink watch died", "err", err)
		}
		w.healthy.Store(false)
		w.kick()
		select {
		case <-w.done:
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (w *Watcher) subscribeOnce() error {
	linkCh := make(chan netlink.LinkUpdate, 64)
	routeCh := make(chan netlink.RouteUpdate, 64)
	done := make(chan struct{})
	defer close(done)

	var errCount atomic.Int32
	onErr := func(err error) {
		w.log.Warn("netlink error", "err", err)
		if errCount.Add(1) >= 3 {
			w.healthy.Store(false)
		}
	}

	if err := netlink.LinkSubscribeWithOptions(linkCh, done, netlink.LinkSubscribeOptions{
		ErrorCallback:     onErr,
		ReceiveBufferSize: recvBuf,
	}); err != nil {
		return err
	}
	if err := netlink.RouteSubscribeWithOptions(routeCh, done, netlink.RouteSubscribeOptions{
		ErrorCallback:     onErr,
		ReceiveBufferSize: recvBuf,
	}); err != nil {
		return err
	}

	if err := w.DumpFull(); err != nil {
		w.log.Warn("dump after resubscribe", "err", err)
	}
	w.healthy.Store(true)
	w.kick()
	w.log.Info("netlink watch healthy")

	debounce := time.NewTimer(time.Hour)
	if !debounce.Stop() {
		<-debounce.C
	}
	defer debounce.Stop()
	var armed bool
	arm := func() {
		if armed {
			if !debounce.Stop() {
				select {
				case <-debounce.C:
				default:
				}
			}
		}
		armed = true
		debounce.Reset(50 * time.Millisecond)
	}

	for {
		select {
		case <-w.done:
			return nil
		case u, ok := <-linkCh:
			if !ok {
				return fmt.Errorf("link channel closed")
			}
			w.applyLink(u)
			if w.verbose {
				w.log.Debug("link update", "ifindex", u.Attrs().Index, "alias", u.Attrs().Alias)
			}
			arm()
		case u, ok := <-routeCh:
			if !ok {
				return fmt.Errorf("route channel closed")
			}
			w.applyRoute(u)
			if w.verbose {
				w.log.Debug("route update", "type", u.Type, "dst", u.Dst)
			}
			arm()
		case <-debounce.C:
			armed = false
			w.kick()
		}
		if !w.healthy.Load() && errCount.Load() >= 3 {
			return fmt.Errorf("netlink errors")
		}
	}
}

func (w *Watcher) applyLink(u netlink.LinkUpdate) {
	w.mu.Lock()
	defer w.mu.Unlock()
	idx := u.Attrs().Index
	// RTM_DELLINK
	if u.Header.Type == unix.RTM_DELLINK {
		delete(w.links, idx)
		return
	}
	w.links[idx] = linkFrom(u.Link)
}

func (w *Watcher) applyRoute(u netlink.RouteUpdate) {
	rr, ok := routeFrom(u.Route)
	if !ok {
		return
	}
	key := routeKey(rr)
	w.mu.Lock()
	defer w.mu.Unlock()
	if u.Type == unix.RTM_DELROUTE {
		delete(w.routes, key)
		return
	}
	w.routes[key] = rr
}

func linkFrom(l netlink.Link) reconcile.Link {
	a := l.Attrs()
	up := a.OperState == netlink.OperUp ||
		(a.Flags&net.FlagUp != 0 && a.OperState != netlink.OperDown && a.OperState != netlink.OperLowerLayerDown)
	return reconcile.Link{
		Index: a.Index,
		Alias: a.Alias,
		Up:    up,
	}
}

func routeFrom(r netlink.Route) (reconcile.Route, bool) {
	pfx, ok := reconcile.IPNetToPrefix(r.Dst)
	if !ok {
		return reconcile.Route{}, false
	}
	var gw netip.Addr
	if r.Gw != nil {
		gw, _ = netip.AddrFromSlice(r.Gw)
	}
	return reconcile.Route{
		Dst:       pfx,
		LinkIndex: r.LinkIndex,
		Gw:        gw,
		Table:     r.Table,
		Protocol:  int(r.Protocol),
	}, true
}

func routeKey(r reconcile.Route) string {
	return r.Dst.String() + "|" + strconv.Itoa(r.LinkIndex)
}
