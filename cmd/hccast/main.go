// Command hccast advertises on-link FIB routes over embedded GoBGP when
// the egress iface carries a valid health alias.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/Vaelatern/hccast/internal/bgp"
	"github.com/Vaelatern/hccast/internal/config"
	"github.com/Vaelatern/hccast/internal/netwatch"
	"github.com/Vaelatern/hccast/internal/reconcile"
)

func main() {
	cfgPath := flag.String("f", "/etc/hccast.yaml", "config file")
	verbose := flag.Bool("v", false, "verbose (debug) logging")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(1)
	}

	srv, err := bgp.New(cfg, log)
	if err != nil {
		log.Error("start bgp", "err", err)
		os.Exit(1)
	}
	defer srv.Close()

	eng := reconcile.NewEngine(srv, log)
	w := netwatch.New(log, *verbose)
	defer w.Close()

	if err := w.DumpFull(); err != nil {
		log.Error("initial dump", "err", err)
		os.Exit(1)
	}

	var cfgMu sync.RWMutex
	current := cfg

	doReconcile := func(why string) {
		cfgMu.RLock()
		c := current
		cfgMu.RUnlock()
		links, routes := w.Snapshot()
		log.Debug("reconcile", "why", why, "links", len(links), "routes", len(routes), "watch_ok", w.Healthy())
		eng.Reconcile(links, routes, c, time.Now())
	}
	doReconcile("startup")

	go w.RunSubscribe()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)

	interval := cfg.Config.ReconcileInterval
	timer := time.NewTicker(interval)
	defer timer.Stop()

	log.Info("hccast running", "config", *cfgPath, "reconcile", interval)

	for {
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			return
		case <-hup:
			nf, err := config.Load(*cfgPath)
			if err != nil {
				log.Error("config reload failed; keeping previous", "err", err)
				continue
			}
			cfgMu.Lock()
			oldInterval := current.Config.ReconcileInterval
			current = nf
			cfgMu.Unlock()
			if err := srv.ApplyPeers(nf.Config.Peers); err != nil {
				log.Error("peer apply", "err", err)
			}
			if nf.Config.ReconcileInterval != oldInterval {
				timer.Reset(nf.Config.ReconcileInterval)
			}
			log.Info("config reloaded")
			doReconcile("sighup")
		case <-w.Wake():
			doReconcile("netlink")
		case <-timer.C:
			// Full dump heals missed events + date expiry.
			if err := w.DumpFull(); err != nil {
				log.Warn("timer dump", "err", err)
			}
			doReconcile("timer")
		}
	}
}
