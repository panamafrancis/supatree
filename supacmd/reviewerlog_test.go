package supacmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPickSession(t *testing.T) {
	cases := []struct {
		own  string
		live []string
		want string
	}{
		{"st-a", []string{"st-b", "st-a"}, "st-a"},
		{"st-a", []string{"st-b"}, "st-b"}, // outlived its own: serve the next
		{"", []string{"st-b", "st-c"}, "st-b"},
		{"st-a", nil, ""},
	}
	for _, c := range cases {
		if got := pickSession(c.own, c.live); got != c.want {
			t.Errorf("pickSession(%q, %v) = %q, want %q", c.own, c.live, got, c.want)
		}
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Following prints only new reviewer lines, waits for a line's end, and
// carries on across the watcher's rotation.
func TestFollowReviewerLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watch.log")
	write := func(flag int, s string) {
		f, err := os.OpenFile(path, flag|os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(s); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	write(os.O_TRUNC, "[t0] reviewer: old\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out syncBuf
	done := make(chan error, 1)
	go func() { done <- followReviewerLog(ctx, path, &out) }()
	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(out.String(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("never printed %q; got:\n%s", want, out.String())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	time.Sleep(1200 * time.Millisecond) // let it take the starting offset
	write(os.O_APPEND, "[t1] round failed: x\n[t1] reviewer: new\n[t1] reviewer: hal")
	waitFor("reviewer: new")
	write(os.O_APPEND, "f\n")
	waitFor("reviewer: half")

	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	write(os.O_TRUNC, "[t2] reviewer: after rotation\n")
	waitFor("after rotation")

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "old") || strings.Contains(got, "round failed") || strings.Contains(got, "reviewer: hal\n") {
		t.Errorf("printed what it should not have:\n%s", got)
	}
}

func TestIsReviewerLine(t *testing.T) {
	for line, want := range map[string]bool{
		"[t] reviewer: created rv-x for o/api#1":                      true,
		"[t] no supatree sessions left — staying up for the reviewer": true,
		"[t] launch lead-748:reviewer in st-main: woken":              false,
		"reviewer: no timestamp":                                      false,
	} {
		if got := isReviewerLine(line); got != want {
			t.Errorf("isReviewerLine(%q) = %v", line, got)
		}
	}
}
