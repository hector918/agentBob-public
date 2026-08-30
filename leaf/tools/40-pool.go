package tools

import (
	"context"
	"sync"
	"time"

	"agentbob/contract"
)

// channelIdle is how long an unused channel lingers before the sweep closes it.
const channelIdle = 10 * time.Minute

// pool is the contract.ChannelPool: the keeper of stateful channels (exec
// sessions; remote connections in slice 3). It hands out a live channel per key
// or builds one, closes a principal's channels on Recycle (warrant Cut), and
// sweeps idle ones on its own timer (a runtime resource — module-self-managed,
// NOT the housekeeping bucket).
type pool struct {
	mu     sync.Mutex
	chans  map[contract.ChannelKey]*pooled
	cancel context.CancelFunc
	done   chan struct{}
}

type pooled struct {
	ch       contract.Channel
	lastUsed time.Time
}

func newPool() *pool {
	return &pool{chans: map[contract.ChannelKey]*pooled{}, done: make(chan struct{})}
}

// live returns the number of pooled channels — the webui tools panel surfaces it
// as a stat (stateful exec/remote sessions currently kept warm).
func (p *pool) live() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.chans)
}

// Acquire returns the live channel for key, or builds + stores one. build() runs
// OUTSIDE the pool lock so a blocking dial can't freeze the whole pool.
func (p *pool) Acquire(key contract.ChannelKey, build func() (contract.Channel, error)) (contract.Channel, error) {
	// Fast path: an alive pooled channel.
	p.mu.Lock()
	if e, ok := p.chans[key]; ok && e.ch.Alive() {
		e.lastUsed = time.Now()
		ch := e.ch
		p.mu.Unlock()
		return ch, nil
	}
	p.mu.Unlock()

	// Build OUTSIDE the lock: build() may block on a network dial (a remote SSH exec
	// channel), and holding p.mu across it would freeze EVERY other pool op — other
	// Acquire, the idle sweep, and Recycle/Cut (permission revocation) — until the
	// dial returns or times out.
	ch, err := build()
	if err != nil {
		return nil, err
	}

	// Re-lock + double-check: a concurrent Acquire for the same key may have built
	// and stored its channel while we dialed. If so, keep the stored one and close
	// ours (one winner per key); the loser is closed OUTSIDE the lock (Close may do
	// network I/O too).
	p.mu.Lock()
	if e, ok := p.chans[key]; ok && e.ch.Alive() {
		e.lastUsed = time.Now()
		winner := e.ch
		p.mu.Unlock()
		_ = ch.Close()
		return winner, nil
	}
	// An existing-but-dead entry is being replaced here: capture its channel so it is
	// Closed OUTSIDE the lock (mirrors the loser-close above / sweepIdle's evictions) —
	// otherwise the dead channel is silently overwritten with no Close.
	var dead contract.Channel
	if e, ok := p.chans[key]; ok {
		dead = e.ch
	}
	p.chans[key] = &pooled{ch: ch, lastUsed: time.Now()}
	p.mu.Unlock()
	if dead != nil {
		_ = dead.Close() // Close OUTSIDE the lock — may do network I/O
	}
	return ch, nil
}

// Recycle closes + drops every channel for a principal (warrant Cut).
func (p *pool) Recycle(identity string) {
	p.mu.Lock()
	var toClose []contract.Channel
	for key, e := range p.chans {
		if key.Identity == identity {
			toClose = append(toClose, e.ch)
			delete(p.chans, key)
		}
	}
	p.mu.Unlock()
	for _, ch := range toClose {
		_ = ch.Close() // Close OUTSIDE the lock — may do network I/O (same as Acquire's loser)
	}
}

func (p *pool) start() {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	go p.sweepLoop(ctx)
}

func (p *pool) sweepLoop(ctx context.Context) {
	defer close(p.done)
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			p.closeAll()
			return
		case <-t.C:
			p.sweepIdle()
		}
	}
}

func (p *pool) sweepIdle() {
	cutoff := time.Now().Add(-channelIdle)
	p.mu.Lock()
	var toClose []contract.Channel
	for key, e := range p.chans {
		if e.lastUsed.Before(cutoff) || !e.ch.Alive() {
			toClose = append(toClose, e.ch)
			delete(p.chans, key)
		}
	}
	p.mu.Unlock()
	for _, ch := range toClose {
		_ = ch.Close() // Close OUTSIDE the lock (network I/O)
	}
}

func (p *pool) closeAll() {
	p.mu.Lock()
	var toClose []contract.Channel
	for key, e := range p.chans {
		toClose = append(toClose, e.ch)
		delete(p.chans, key)
	}
	p.mu.Unlock()
	for _, ch := range toClose {
		_ = ch.Close() // Close OUTSIDE the lock (network I/O)
	}
}

func (p *pool) stop() {
	if p.cancel == nil {
		return
	}
	p.cancel()
	<-p.done
}

var _ contract.ChannelPool = (*pool)(nil)
