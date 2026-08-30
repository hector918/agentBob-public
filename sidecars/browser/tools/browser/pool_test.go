package browser

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"agentbob/sidecars/browser/config"
	"agentbob/sidecars/browser/core"
	"agentbob/sidecars/browser/tools/file"
)

// TestConsoleRingDrainExitsOnDone verifies the goroutine-leak fix: the
// drainer started by newConsoleRing must return once its `done` channel
// fires (mirroring browserCtx cancellation in CloseInstance), instead of
// blocking forever on `for range r.in`. We assert via the live goroutine
// count delta around create→stop.
func TestConsoleRingDrainExitsOnDone(t *testing.T) {
	before := runtime.NumGoroutine()

	done := make(chan struct{})
	r := newConsoleRing(8, done)

	// Drainer should be running now; push a couple entries through it.
	r.push(ConsoleEntry{Level: "log", Text: "a"})
	r.push(ConsoleEntry{Level: "error", Text: "b"})

	// Signal stop (what CloseInstance's browserCxl() does to browserCtx).
	close(done)

	// Wait (bounded) for the goroutine count to fall back to baseline.
	deadline := time.Now().Add(2 * time.Second)
	for {
		runtime.Gosched()
		if runtime.NumGoroutine() <= before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("drain goroutine did not exit after done closed: goroutines before=%d now=%d",
				before, runtime.NumGoroutine())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestConsoleRingPushNoPanicAfterDone confirms push() stays panic-free
// after the drainer has stopped. Because the fix uses a `done` signal and
// never closes `r.in`, the non-blocking send simply fills the buffer (then
// drops) rather than panicking on a closed channel.
func TestConsoleRingPushNoPanicAfterDone(t *testing.T) {
	done := make(chan struct{})
	r := newConsoleRing(8, done)
	close(done)
	// Give the drainer a moment to exit so sends accumulate in the buffer.
	time.Sleep(20 * time.Millisecond)

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("push panicked after drain stopped: %v", rec)
		}
	}()
	// Far exceed the intake buffer (1024) to exercise both the buffered-
	// send and the drop branch; neither may panic.
	for i := 0; i < 2000; i++ {
		r.push(ConsoleEntry{Level: "log", Text: "x"})
	}
}

// TestSpawnLockForStable verifies the #3 fix: the per-key spawn mutex is
// stable (the same *sync.Mutex on every call for a key) because Acquire no
// longer deletes it. A churning entry was what let a launch failure +
// concurrent re-acquire spawn two chromium on one user-data-dir.
func TestSpawnLockForStable(t *testing.T) {
	p := NewPool(config.BrowserConfig{}, config.FilesystemConfig{}, false)
	m1 := p.spawnLockFor("tg:group:7")
	m2 := p.spawnLockFor("tg:group:7")
	if m1 != m2 {
		t.Fatalf("spawnLockFor returned different mutexes for the same key — per-key serialisation broken")
	}
	if other := p.spawnLockFor("tg:group:8"); other == m1 {
		t.Fatalf("spawnLockFor returned the same mutex for distinct keys")
	}
}

// TestCloseInstanceUnknownKeyNoPanic guards the idempotent close path the
// drain-stop change rides on: closing a key with no live session is a
// no-op, never a panic.
func TestCloseInstanceUnknownKeyNoPanic(t *testing.T) {
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("CloseInstance on unknown key panicked: %v", rec)
		}
	}()
	p := NewPool(config.BrowserConfig{}, config.FilesystemConfig{}, false)
	p.CloseInstance("never-spawned")
}

// TestAutoResumeNoSSRFGate is the SSRF-removal fix: the auto-resume path
// must NOT run a code-layer SSRF/host check (SSRF defense lives at the
// network layer, not in bob code). The asymmetric gate that previously
// rejected private/loopback URLs on resume — while the model-controlled
// Navigate entry had no such gate — is removed so both paths are
// consistent. We assert at the resume read seam (readLastNavURL, the
// last filter before the resume navigate) that a loopback / link-local
// metadata URL is returned unchanged rather than rejected.
func TestAutoResumeNoSSRFGate(t *testing.T) {
	dir := t.TempDir()
	for _, u := range []string{
		"http://127.0.0.1:8080/admin",
		"http://169.254.169.254/latest/meta-data/", // cloud metadata
		"http://192.168.1.100:11434/v1",            // RFC1918 (bob's own llama)
		"http://localhost/",
	} {
		if err := os.WriteFile(filepath.Join(dir, lastNavMarker), []byte(u), 0o644); err != nil {
			t.Fatalf("write marker: %v", err)
		}
		if got := readLastNavURL(dir); got != u {
			t.Fatalf("auto-resume read rejected/altered private URL %q (got %q) — an SSRF gate is still on the resume path", u, got)
		}
	}
}

// TestNoWebSSRFImport reinforces the SSRF-removal fix structurally: the
// browser pool no longer imports the sidecar's tools/web at all, so no code-
// layer SSRF helper (web.CheckURLHost / web.HostFromTarget) can be on the
// auto-resume path. This catches a re-introduction the behavioural test
// above would miss if the gate were re-added on a different seam.
func TestNoWebSSRFImport(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "pool.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse pool.go: %v", err)
	}
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if path == "agentbob/sidecars/browser/tools/web" {
			t.Fatalf("pool.go still imports tools/web — the code-layer SSRF gate must be removed from the auto-resume path")
		}
	}
}

// TestAutoResumeSerializedBeforePublish is the concurrency fix: a session
// that is mid-auto-resume must NOT be reachable by a concurrent Acquire,
// or two goroutines would issue overlapping chromedp.Run calls on one
// browserCtx (CDP is not safe for concurrent commands on a single target).
// The fix moves the resume navigate to BEFORE the session is published into
// p.sessions (still under spawnLock). We can't launch real chromium in a
// unit test, so we exercise the pure lock/state seam that Acquire relies
// on: while the spawn lock is held and the session is not yet in the map,
// no concurrent reader may observe it; once published, it becomes visible.
// Run with -race to catch any unsynchronised map access on this seam.
func TestAutoResumeSerializedBeforePublish(t *testing.T) {
	p := NewPool(config.BrowserConfig{}, config.FilesystemConfig{}, false)
	const key = "tg:group:resume"

	// Stand-in for the constructed-but-not-yet-published Session.
	sess := &Session{}

	spawnLock := p.spawnLockFor(key)
	spawnLock.Lock()

	resumeStarted := make(chan struct{})
	resumeDone := make(chan struct{})
	published := make(chan struct{})

	// Spawner: holds spawnLock, "auto-resumes" (simulated by a delay during
	// which the session must stay invisible), THEN publishes to the map —
	// mirroring the relocated resume-before-insert ordering in Acquire.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer spawnLock.Unlock()
		close(resumeStarted)
		// Simulated resume navigate window.
		time.Sleep(40 * time.Millisecond)
		close(resumeDone)
		p.mu.Lock()
		p.sessions[key] = sess
		p.mu.Unlock()
		close(published)
	}()

	<-resumeStarted

	// Concurrent reader: emulates another Acquire's fast path probing the
	// map. It must NOT see the session until the resume window has closed
	// and the session is published.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			p.mu.Lock()
			s, ok := p.sessions[key]
			p.mu.Unlock()
			if ok {
				// First visibility MUST be after resume completed.
				select {
				case <-resumeDone:
				default:
					t.Errorf("concurrent Acquire saw the session mid-auto-resume — overlapping chromedp.Run possible")
				}
				if s != sess {
					t.Errorf("unexpected session instance from map")
				}
				return
			}
			runtime.Gosched()
		}
	}()

	<-published
	wg.Wait()
}

// TestSessionSandboxMatchesFileResolve is the #2 invariant: the dir
// browser_snapshot(format=image) writes into must equal the dir read_file
// resolves the returned relative path against, under custom SandboxRoot and
// namespace_by_bot. Pool.sessionSandbox is the exact computation
// browserSnapshot.Run feeds to Snapshot, which then appends "screenshots".
func TestSessionSandboxMatchesFileResolve(t *testing.T) {
	cases := []struct {
		name           string
		sandboxRoot    string
		namespaceByBot bool
		botID          string
		scope          string
	}{
		{"default-root", "", false, "", "tg:group:42"},
		{"custom-root", "/srv/bobsandbox", false, "", "tg:group:42"},
		{"namespace-by-bot", "/srv/bobsandbox", true, "botA", "tg:group:42"},
		{"namespace-by-bot-no-botid", "/srv/bobsandbox", true, "", "tg:group:42"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsCfg := config.FilesystemConfig{SandboxRoot: tc.sandboxRoot}
			p := NewPool(config.BrowserConfig{}, fsCfg, tc.namespaceByBot)
			actx := core.AgentCtx{
				SessionScope: tc.scope,
				Event:        core.MessageEvent{BotID: tc.botID},
			}

			// What browserSnapshot.Run passes to Snapshot.
			gotBase := p.sessionSandbox(actx)
			// What read_file resolves a relative path against.
			wantBase := file.SessionSandbox(fsCfg, tc.namespaceByBot, actx)
			if gotBase != wantBase {
				t.Fatalf("sessionSandbox mismatch:\n got  %s\n want %s", gotBase, wantBase)
			}

			// And the full screenshots dir Snapshot writes into must be the
			// resolve dir + "screenshots" (where the relative path
			// "screenshots/snap-*.png" lands).
			gotShots := filepath.Join(gotBase, "screenshots")
			wantShots := filepath.Join(file.SessionSandbox(fsCfg, tc.namespaceByBot, actx), "screenshots")
			if gotShots != wantShots {
				t.Fatalf("screenshots dir mismatch:\n got  %s\n want %s", gotShots, wantShots)
			}
		})
	}
}

// TestControlledCloseKeepsCopyOnWritebackFailure pins the BP-2 fix: when a controlled
// copy's close-time disk write-back FAILS (e.g. disk full), CloseInstance must KEEP the
// copy dir — removing it would destroy the flushed login storage the master never
// received. On a SUCCESSFUL write-back the copy dir is discarded as before. Sessions are
// faked (no chromium): loginSaved=true skips the CDP cookie capture, and the graceful
// close on a plain context fails fast (logged + continued).
func TestControlledCloseKeepsCopyOnWritebackFailure(t *testing.T) {
	newFakeCopy := func(masterDir string) (*Session, string) {
		copyDir := filepath.Join(t.TempDir(), "copy")
		if err := os.MkdirAll(copyDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(copyDir, "Cookies"), []byte("login"), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		return &Session{
			rootCtx:       ctx,
			BrowserCtx:    ctx,
			browserCxl:    cancel,
			allocCancel:   cancel,
			dataDir:       copyDir,
			masterDir:     masterDir,
			ephemeralCopy: true,
			controlled:    true,
			loginSaved:    true, // skip the CDP cookie re-capture (no live chromium here)
		}, copyDir
	}

	p := NewPool(config.BrowserConfig{}, config.FilesystemConfig{}, false)
	defer p.CloseAll()

	// FAILURE: the master's parent is a regular FILE, so the write-back staging mkdir
	// fails (ENOTDIR — works even as root, unlike permission tricks). Copy must survive.
	notADir := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, copyDir := newFakeCopy(filepath.Join(notADir, "master"))
	p.mu.Lock()
	p.sessions["browsercopy_fail"] = s
	p.mu.Unlock()
	p.CloseInstance("browsercopy_fail")
	if _, err := os.Stat(copyDir); err != nil {
		t.Fatalf("copy dir must be KEPT after a failed write-back, stat: %v", err)
	}

	// SUCCESS: write-back lands in the master; the copy dir is discarded.
	master := filepath.Join(t.TempDir(), "master")
	s2, copyDir2 := newFakeCopy(master)
	p.mu.Lock()
	p.sessions["browsercopy_ok"] = s2
	p.mu.Unlock()
	p.CloseInstance("browsercopy_ok")
	if _, err := os.Stat(filepath.Join(master, "Cookies")); err != nil {
		t.Fatalf("successful write-back must install the master, stat: %v", err)
	}
	if _, err := os.Stat(copyDir2); !os.IsNotExist(err) {
		t.Fatalf("copy dir must be discarded after a successful write-back, stat err=%v", err)
	}
}
