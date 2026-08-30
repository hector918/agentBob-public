package local

import (
	"fmt"
	"os"
	"sync"
	"time"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinner is a one-character "thinking…" indicator: it prints a single braille
// char (after whatever is already on the current line) and animates it in place
// using backspaces, so erasing it touches only that one character. It is a no-op
// when stdout is not a terminal (piped/redirected output).
type spinner struct {
	active bool
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once
}

func startSpinner() *spinner {
	s := &spinner{stop: make(chan struct{}), done: make(chan struct{})}
	if !stdoutIsTTY() {
		close(s.done) // nothing to animate; erase() will be a no-op
		return s
	}
	s.active = true
	fmt.Print(spinnerFrames[0])
	go func() {
		defer close(s.done)
		t := time.NewTicker(120 * time.Millisecond)
		defer t.Stop()
		i := 0
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
				i = (i + 1) % len(spinnerFrames)
				fmt.Print("\b" + spinnerFrames[i])
			}
		}
	}()
	return s
}

// erase stops the animation and removes the spinner character. Idempotent — the
// visual erase must run AT MOST once, or a second call (e.g. the next streamed
// chunk) backspaces over real output. The whole body is under once.Do so repeat
// calls are true no-ops. A nil receiver is a no-op (render rounds that never
// started a spinner).
func (s *spinner) erase() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		close(s.stop)
		<-s.done
		if s.active {
			fmt.Print("\b \b") // backspace, overwrite with a space, backspace
		}
	})
}

func stdoutIsTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
