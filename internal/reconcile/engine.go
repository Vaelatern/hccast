package reconcile

import (
	"log/slog"
	"sync"
	"time"

	"github.com/Vaelatern/hccast/internal/config"
)

// Advertiser applies path adds/deletes. Implemented by internal/bgp.
type Advertiser interface {
	AddPath(prefix string, attrs config.PathAttrs) error
	DeletePath(prefix string) error
}

// Engine holds the in-memory "have" set and diffs against Desired.
type Engine struct {
	mu   sync.Mutex
	have map[string]config.PathAttrs
	adv  Advertiser
	log  *slog.Logger
}

func NewEngine(adv Advertiser, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	return &Engine{have: make(map[string]config.PathAttrs), adv: adv, log: log}
}

// Reconcile dumps → Desired → Add/Delete/Replace.
func (e *Engine) Reconcile(links []Link, routes []Route, cfg *config.File, now time.Time) {
	desired := Desired(links, routes, cfg, now)

	e.mu.Lock()
	defer e.mu.Unlock()

	for pfx, attrs := range desired {
		old, ok := e.have[pfx]
		if !ok {
			if err := e.adv.AddPath(pfx, attrs); err != nil {
				e.log.Error("add path", "prefix", pfx, "err", err)
				continue
			}
			e.log.Info("advertise", "prefix", pfx)
			e.have[pfx] = attrs
			continue
		}
		if !old.Equal(attrs) {
			// replace: delete then add
			if err := e.adv.DeletePath(pfx); err != nil {
				e.log.Error("replace delete", "prefix", pfx, "err", err)
				continue
			}
			if err := e.adv.AddPath(pfx, attrs); err != nil {
				e.log.Error("replace add", "prefix", pfx, "err", err)
				delete(e.have, pfx)
				continue
			}
			e.log.Info("replace", "prefix", pfx)
			e.have[pfx] = attrs
		}
	}
	for pfx := range e.have {
		if _, ok := desired[pfx]; ok {
			continue
		}
		if err := e.adv.DeletePath(pfx); err != nil {
			e.log.Error("withdraw", "prefix", pfx, "err", err)
			continue
		}
		e.log.Info("withdraw", "prefix", pfx)
		delete(e.have, pfx)
	}
}
