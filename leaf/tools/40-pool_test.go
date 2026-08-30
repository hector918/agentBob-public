package tools

import (
	"testing"

	"agentbob/contract"
)

type fakeChan struct {
	alive  bool
	closed bool
}

func (c *fakeChan) Alive() bool  { return c.alive }
func (c *fakeChan) Close() error { c.closed = true; c.alive = false; return nil }

func TestPoolAcquireReuseRecycle(t *testing.T) {
	p := newPool() // no sweep goroutine started — drive Acquire/Recycle directly
	key := contract.ChannelKey{Identity: "a", Space: "s", Kind: "exec"}

	builds := 0
	build := func() (contract.Channel, error) {
		builds++
		return &fakeChan{alive: true}, nil
	}

	c1, _ := p.Acquire(key, build)
	c2, _ := p.Acquire(key, build)
	if builds != 1 {
		t.Errorf("a live channel should be reused, built %d times", builds)
	}
	if c1 != c2 {
		t.Error("Acquire should return the same channel for the same key")
	}

	// A dead channel is rebuilt.
	c1.(*fakeChan).alive = false
	c3, _ := p.Acquire(key, build)
	if builds != 2 || c3 == c1 {
		t.Errorf("a dead channel should rebuild (builds=%d)", builds)
	}

	// Recycle(identity) closes + drops.
	p.Recycle("a")
	if !c3.(*fakeChan).closed {
		t.Error("Recycle should close the principal's channel")
	}
	p.Acquire(key, build)
	if builds != 3 {
		t.Errorf("after recycle Acquire should rebuild (builds=%d)", builds)
	}
}
