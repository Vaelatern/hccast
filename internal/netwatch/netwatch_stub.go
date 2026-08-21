//go:build !linux

package netwatch

import (
	"fmt"
	"log/slog"

	"github.com/Vaelatern/hccast/internal/reconcile"
)

type Watcher struct {
	log     *slog.Logger
	wake    chan struct{}
	healthy bool
}

func New(log *slog.Logger, verbose bool) *Watcher {
	return &Watcher{log: log, wake: make(chan struct{}), healthy: false}
}
func (w *Watcher) Wake() <-chan struct{}                 { return w.wake }
func (w *Watcher) Healthy() bool                         { return false }
func (w *Watcher) Close()                                {}
func (w *Watcher) Snapshot() ([]reconcile.Link, []reconcile.Route) {
	return nil, nil
}
func (w *Watcher) DumpFull() error {
	return fmt.Errorf("netwatch: linux only")
}
func (w *Watcher) RunSubscribe() {}
