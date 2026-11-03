package supatree

import (
	"strings"
	"testing"
)

func TestWithChangeBlock(t *testing.T) {
	first := ChangeBlock("fix-login", []string{"https://github.com/o/api/pull/1"})
	body := WithChangeBlock("Fixes the login race.", first)
	if !strings.HasPrefix(body, "Fixes the login race.\n\n<!-- supatree:change id=fix-login -->") {
		t.Fatalf("body = %q", body)
	}
	// Rewriting replaces the block rather than stacking a second one.
	second := ChangeBlock("fix-login", []string{"https://github.com/o/api/pull/1", "https://github.com/o/web/pull/2"})
	again := WithChangeBlock(body, second)
	if strings.Count(again, "supatree:change id=") != 1 || !strings.Contains(again, "o/web/pull/2") || !strings.HasPrefix(again, "Fixes the login race.") {
		t.Errorf("rewritten body = %q", again)
	}
	if got := WithChangeBlock("", first); got != first {
		t.Errorf("empty body = %q, want just the block", got)
	}
	if WithChangeBlock(again, second) != again {
		t.Error("writing the same block twice changed the body")
	}
}

func TestPRURLIn(t *testing.T) {
	out := "Creating pull request for st/x/api into main in o/api\n\nhttps://github.com/o/api/pull/12\n"
	if got := prURLIn(out); got != "https://github.com/o/api/pull/12" {
		t.Errorf("url = %q", got)
	}
}
