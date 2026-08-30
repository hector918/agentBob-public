package warrant

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"agentbob/contract"

	"golang.org/x/crypto/ssh"
)

// sshExecChannel is a remote shell over a POOLED ssh connection. The expensive
// part (the tcp+ssh handshake) is the connection, reused across Run calls; each
// Run opens a fresh ssh session for one command. Like the local one-shot, cwd/env
// don't persist across commands — the pooled CONNECTION is what the pool saves.
// This is the pool's first truly-stateful consumer.
type sshExecChannel struct {
	client *ssh.Client
	mu     sync.Mutex // serialize Run
	alive  atomic.Bool
	once   sync.Once
}

func newSSHExecChannel(client *ssh.Client) *sshExecChannel {
	c := &sshExecChannel{client: client}
	c.alive.Store(true)
	return c
}

func (c *sshExecChannel) Alive() bool { return c.alive.Load() }

func (c *sshExecChannel) Close() error {
	c.once.Do(func() {
		c.alive.Store(false)
		_ = c.client.Close()
	})
	return nil
}

func (c *sshExecChannel) Run(ctx context.Context, command string) (contract.ExecResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.alive.Load() {
		return contract.ExecResult{}, fmt.Errorf("ssh session is not alive")
	}
	sess, err := c.client.NewSession()
	if err != nil {
		c.Close() // a dead connection — drop it so the pool rebuilds
		return contract.ExecResult{}, fmt.Errorf("ssh new session: %w", err)
	}
	defer sess.Close()

	// Cap combined output as it streams (see cappedWriter) instead of CombinedOutput's
	// unbounded buffer — a remote command flooding stdout could otherwise OOM bob.
	w := &cappedWriter{}
	sess.Stdout = w
	sess.Stderr = w
	resCh := make(chan error, 1)
	go func() {
		resCh <- sess.Run(command)
	}()

	cctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	select {
	case <-cctx.Done():
		_ = sess.Close() // abort the remote command
		if ctx.Err() != nil {
			return contract.ExecResult{}, ctx.Err()
		}
		return contract.ExecResult{ExitCode: -1, Truncated: true}, fmt.Errorf("command timed out after %s", execTimeout)
	case runErr := <-resCh:
		exit := 0
		if ee, ok := runErr.(*ssh.ExitError); ok {
			exit = ee.ExitStatus()
		} else if runErr != nil {
			return contract.ExecResult{}, fmt.Errorf("ssh run: %w", runErr)
		}
		s, trunc := w.result()
		return contract.ExecResult{Output: s, ExitCode: exit, Truncated: trunc}, nil
	}
}

var _ contract.ExecChannel = (*sshExecChannel)(nil)
