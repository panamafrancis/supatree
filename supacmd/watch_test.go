package supacmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/panamafrancis/supatree/pkg/supatree"
	"github.com/panamafrancis/workbench/pkg/testutil"
)

// A repeating error must not grow watch.log without bound: it rotates to one
// previous generation, and the line that tripped the rotation is not lost.
func TestWatchLogRotates(t *testing.T) {
	testutil.IsolateHome(t)
	openWatchLog()
	defer closeWatchLog()
	if watchLog.f == nil {
		t.Fatal("watch log did not open")
	}

	line := strings.Repeat("x", 1000)
	for i := 0; i < (watchLogMax/1000)+50; i++ {
		logWatch("%s", line)
	}
	logWatch("last line")

	cur, err := os.ReadFile(watchLogPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(cur) > watchLogMax {
		t.Errorf("watch.log is %d bytes, over the %d cap", len(cur), watchLogMax)
	}
	if !strings.Contains(string(cur), "last line") {
		t.Error("the newest line is not in the current log")
	}
	if st, err := os.Stat(watchLogPath() + ".1"); err != nil || st.Size() == 0 {
		t.Errorf("no previous generation kept (%v)", err)
	}
}

// An upgraded binary restarts the watcher, but only once it has stopped
// changing: a binary still being copied must not be exec'd.
func TestBinaryWatchWaitsForAStableReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "supatree")
	write := func(body string, mod time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().Add(-time.Hour)
	write("old", base)
	stamp, _ := statBinary(path)
	w := &binaryWatch{path: path, stamp: stamp}

	if w.replaced() {
		t.Fatal("an unchanged binary is not a replacement")
	}
	write("new, half", base.Add(time.Minute))
	if w.replaced() {
		t.Fatal("restarted on the first sight of a change")
	}
	write("new, complete", base.Add(2*time.Minute))
	if w.replaced() {
		t.Fatal("restarted while the binary was still changing")
	}
	if !w.replaced() {
		t.Fatal("a replacement stable across two checks should restart the watcher")
	}
}

// A background open is answered at its outcome line, not when the child
// exits: the child goes on to watch the focus, and the queue must not wait.
func TestRunBackgroundOpenReturnsAtOutcome(t *testing.T) {
	script := filepath.Join(t.TempDir(), "open")
	body := "#!/bin/sh\necho 'supatree-open-outcome: " + string(supatree.LaunchStarted) + "'\nsleep 5\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	outcome, detail := runBackgroundOpen(context.Background(), script)
	if outcome != supatree.LaunchStarted || detail != "" {
		t.Fatalf("got %q, %q; want started", outcome, detail)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("waited %v for a child that had already answered", d)
	}
}

// A child that fails is read to its exit, and its error is the detail.
func TestRunBackgroundOpenReportsFailure(t *testing.T) {
	script := filepath.Join(t.TempDir(), "open")
	body := "#!/bin/sh\necho 'supatree-open-outcome: " + string(supatree.LaunchFailed) + "'\necho 'nono refused' >&2\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	outcome, detail := runBackgroundOpen(context.Background(), script)
	if outcome != supatree.LaunchFailed || !strings.Contains(detail, "nono refused") {
		t.Fatalf("got %q, %q; want failed with the child's error", outcome, detail)
	}
}

// The watcher leaves the directory it was started in, which may be a tree
// about to be removed, for the state root.
func TestLeaveCallerDir(t *testing.T) {
	testutil.IsolateHome(t)
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.MkdirAll(supatree.StateRoot(), 0o755); err != nil {
		t.Fatal(err)
	}
	doomed := t.TempDir()
	if err := os.Chdir(doomed); err != nil {
		t.Fatal(err)
	}
	leaveCallerDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(supatree.StateRoot())
	if got, _ := filepath.EvalSymlinks(cwd); got != want {
		t.Errorf("cwd = %s, want the state root %s", got, want)
	}
}

// A restart waits for a running op only so long: an op stuck on a dead
// connection once wedged the whole watcher behind it.
func TestRestartWatcherGivesUpOnAHeldOpLock(t *testing.T) {
	old := restartLockWait
	restartLockWait = 300 * time.Millisecond
	defer func() { restartLockWait, restartDeferred = old, false }()
	opsMu.Lock()
	defer opsMu.Unlock()
	done := make(chan struct{})
	go func() { restartWatcher("/nonexistent/supatree"); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("restartWatcher blocked on the op lock")
	}
	if !restartDeferred {
		t.Error("deferral not recorded")
	}
}
