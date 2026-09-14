package bili

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

// GateConfig controls the deliberately conservative request cadence. Zero
// values are filled with the production defaults by NewRequestGate.
type GateConfig struct {
	Disabled       bool
	MinDelay       time.Duration
	MaxDelay       time.Duration
	EveryRequests  int
	ExtraMinDelay  time.Duration
	ExtraMaxDelay  time.Duration
	FirstImmediate bool
}

func DefaultGateConfig() GateConfig {
	return GateConfig{
		MinDelay: 2 * time.Second, MaxDelay: 6 * time.Second,
		EveryRequests: 30,
		ExtraMinDelay: 15 * time.Second, ExtraMaxDelay: 45 * time.Second,
		FirstImmediate: true,
	}
}

// RequestGate serializes the start of HTTP requests. A cancelled context
// always leaves the gate usable for the next caller.
type RequestGate struct {
	mu       sync.Mutex
	cfg      GateConfig
	rng      *rand.Rand
	requests int
}

func NewRequestGate(cfg GateConfig) *RequestGate {
	if cfg.Disabled {
		return &RequestGate{cfg: cfg, rng: rand.New(rand.NewPCG(1, 2))}
	}
	defaults := DefaultGateConfig()
	if cfg.MinDelay == 0 && cfg.MaxDelay == 0 && cfg.EveryRequests == 0 && cfg.ExtraMinDelay == 0 && cfg.ExtraMaxDelay == 0 {
		cfg.FirstImmediate = defaults.FirstImmediate
	}
	if cfg.MinDelay == 0 && cfg.MaxDelay == 0 {
		cfg.MinDelay, cfg.MaxDelay = defaults.MinDelay, defaults.MaxDelay
	}
	if cfg.EveryRequests == 0 {
		cfg.EveryRequests = defaults.EveryRequests
	}
	if cfg.ExtraMinDelay == 0 && cfg.ExtraMaxDelay == 0 {
		cfg.ExtraMinDelay, cfg.ExtraMaxDelay = defaults.ExtraMinDelay, defaults.ExtraMaxDelay
	}
	if cfg.MaxDelay < cfg.MinDelay {
		cfg.MaxDelay = cfg.MinDelay
	}
	if cfg.ExtraMaxDelay < cfg.ExtraMinDelay {
		cfg.ExtraMaxDelay = cfg.ExtraMinDelay
	}
	return &RequestGate{cfg: cfg, rng: rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(time.Now().UnixMicro())))}
}

// Wait waits for this request's assigned polite delay. Calls are serialized,
// including the wait itself, which prevents concurrent callers from bursting.
func (g *RequestGate) Wait(ctx context.Context) error {
	if g.cfg.Disabled {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			g.mu.Lock()
			g.requests++
			g.mu.Unlock()
			return nil
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	delay := time.Duration(0)
	if !(g.requests == 0 && g.cfg.FirstImmediate) {
		delay += g.randomDuration(g.cfg.MinDelay, g.cfg.MaxDelay)
	}
	next := g.requests + 1
	if g.cfg.EveryRequests > 0 && next%g.cfg.EveryRequests == 0 {
		delay += g.randomDuration(g.cfg.ExtraMinDelay, g.cfg.ExtraMaxDelay)
	}
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	g.requests++
	return nil
}

func (g *RequestGate) randomDuration(min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	return min + time.Duration(g.rng.Int64N(int64(max-min)+1))
}
