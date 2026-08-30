package warrant

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"agentbob/contract"
)

// execTimeout caps one command. execMaxOutput caps the combined output fed back.
const (
	execTimeout   = 2 * time.Minute
	execMaxOutput = 16 * 1024
)

// localExecChannel runs shell commands in a space's local directory. Each Run is
// a fresh `bash -c` rooted at the space dir, so the FILES in the space persist
// across commands (and are shared with the space's file channel — write-then-run
// works), but cwd/env do NOT persist across calls.
//
// NOTE (doc §12): the unified design wants a CONTINUOUS session (cwd/env persist)
// — that needs a pty (bash block-buffers stdout to a pipe, so a sentinel-on-a-
// pipe stalls; and an interactive bash on a pty brings prompt/echo handling). The
// one-shot model is the skeleton-proven baseline; the pty (or a remote ssh
// session, slice 3) is the continuous upgrade behind this same ExecChannel. The
// channel is pooled all the same — the holder is reused; slice 3's remote session
// is the first truly-stateful pooled channel.
type localExecChannel struct {
	dir  string // cwd — the user's space dir (files persist here)
	home string // HOME/TMPDIR — a private dir OUTSIDE the space, so programs' dotdirs
	// (.cache/.local/…) and temp files don't pollute the user-visible file space.
	mu sync.Mutex // serialize Run on this channel
}

func newLocalExecChannel(space, dir, home string) (*localExecChannel, error) {
	// Scrub the $BOB_HOME-rooted abs path out of any mkdir error — these surface to the
	// model through the terminal tool (info hygiene, same as the file channel).
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("warrant: prepare space %q: %w", space, scrubRoot(err, dir))
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, fmt.Errorf("warrant: prepare exec-home for %q: %w", space, scrubRoot(err, home))
	}
	return &localExecChannel{
		dir:  dir,
		home: home,
	}, nil
}

func (c *localExecChannel) Alive() bool  { return true }
func (c *localExecChannel) Close() error { return nil }

func (c *localExecChannel) Run(ctx context.Context, command string) (contract.ExecResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "bash", "-c", command)
	cmd.Dir = c.dir // cwd = the space (files the bot creates persist + are visible via fs)
	// Build the env from a WHITELIST, never os.Environ() — docs/security-assumptions.md
	// §5.2 invariant: bob's DSN / API keys / bot tokens (all read from the process
	// env) MUST NOT be inherited by a model-controlled shell. HOME/TMPDIR are pinned
	// to a PRIVATE dir (not the space) so a program's ~/.cache, temp files, etc. don't
	// litter the user's file space.
	cmd.Env = sandboxEnv(c.home)
	// Process-group kill: stdout/stderr are pipes (bytes.Buffer), so Wait blocks
	// until pipe-EOF — a backgrounded child (`cmd &`) inheriting the write end
	// would hang Run forever (and wedge c.mu for every later exec on this
	// channel). Setpgid puts the whole tree in one group; Cancel kills the group
	// (negative pid); WaitDelay bounds the pipe-EOF wait in case a descendant
	// escaped the group. Same tri-part fix as leaf/tools/90-scrapling.go.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 5 * time.Second
	// Cap the output AS IT IS PRODUCED, not after: a model-controlled command could
	// emit gigabytes, and buffering it all before capOut runs is an OOM. The capped
	// writer stores only ~execMaxOutput bytes while counting the total for the notice.
	w := &cappedWriter{}
	cmd.Stdout = w
	cmd.Stderr = w

	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		// bash exited 0 but a backgrounded child still holds the pipe write end;
		// WaitDelay abandoned the pipe. The command itself succeeded — return the
		// output captured so far.
		err = nil
	}
	// Parent (turn) deadline first — cctx inherits ctx, so a parent-cancel also trips
	// cctx's DeadlineExceeded; checking cctx first would misreport it as our exec
	// timeout. Mirrors sshExecChannel.Run's order.
	if ctx.Err() != nil {
		return contract.ExecResult{}, ctx.Err()
	}
	if cctx.Err() == context.DeadlineExceeded {
		s, _ := w.result()
		return contract.ExecResult{Output: s, ExitCode: -1, Truncated: true}, fmt.Errorf("command timed out after %s", execTimeout)
	}
	exit := 0
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if err != nil {
		return contract.ExecResult{}, fmt.Errorf("run shell: %w", err)
	}
	s, trunc := w.result()
	return contract.ExecResult{Output: s, ExitCode: exit, Truncated: trunc}, nil
}

// cappedWriter stores at most execMaxOutput bytes of what is written to it while
// counting the TOTAL bytes seen, so a command that floods stdout/stderr can't OOM
// bob before the post-hoc cap ran. Bytes past the cap are dropped on the floor.
// Write is mutex-guarded because crypto/ssh copies stdout+stderr from two separate
// goroutines into the same writer (see sshExecChannel.Run).
type cappedWriter struct {
	mu    sync.Mutex
	buf   []byte
	total int
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.total += len(p)
	if room := execMaxOutput - len(w.buf); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		w.buf = append(w.buf, p...)
	}
	return len(p), nil // "accept" every byte (the tail is discarded, not an error)
}

// result renders the captured output with the same truncation-notice semantics
// capOut produced: untruncated output verbatim, or the first execMaxOutput bytes
// (trimmed to a rune boundary) plus a "…[truncated — X of Y bytes]" footer.
func (w *cappedWriter) result() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.total <= execMaxOutput {
		return string(w.buf), false
	}
	s := string(w.buf)
	// The cap may have sliced the buffer mid-rune. Drop only a trailing INCOMPLETE
	// rune so the kept output is always valid UTF-8; a rune that ended exactly on the
	// cap boundary is complete and stays intact.
	cut := len(s)
	for cut > 0 {
		if r, size := utf8.DecodeLastRuneInString(s[:cut]); r == utf8.RuneError && size <= 1 {
			cut--
			continue
		}
		break
	}
	return s[:cut] + fmt.Sprintf("\n…[truncated — %d of %d bytes]", cut, w.total), true
}

// sandboxEnv builds the exec environment from a WHITELIST, NEVER os.Environ() —
// docs/security-assumptions.md §5.2 + §11 invariant: bob's secrets (DSN / API
// keys / bot tokens, all read from the process env) must not be inherited by a
// model-controlled shell. Only PATH + locale/tz pass through (none secret-bearing);
// HOME/TMPDIR are pinned to the private dir.
func sandboxEnv(home string) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	env := []string{"HOME=" + home, "TMPDIR=" + home, "PATH=" + path}
	for _, k := range []string{"LANG", "LC_ALL", "LC_CTYPE", "TERM", "TZ"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

var _ contract.ExecChannel = (*localExecChannel)(nil)
