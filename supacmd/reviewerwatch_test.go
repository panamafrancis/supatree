package supacmd

import (
	"strings"
	"testing"
)

func TestRenderStreamLine(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","model":"claude-opus-5-5","session_id":"s"}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hidden"},{"type":"text","text":"Reading the diff."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git diff main\nmore"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__supatree__review_post","input":{"event":"COMMENT","member":"api"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"fine"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":[{"type":"text","text":"exit 1\nstack"}]}]}}`,
		`{"type":"result","subtype":"success","duration_ms":125000,"num_turns":12,"total_cost_usd":1.234}`,
		`nono: sandbox ready`,
	}
	var b strings.Builder
	for _, l := range lines {
		renderStreamLine(&b, l)
	}
	want := `── run started (claude-opus-5-5)
Reading the diff.
→ Bash git diff main …
→ review_post event="COMMENT" member="api"
  ✗ exit 1 …
── run finished after 2m5s, 12 turns, $1.23
nono: sandbox ready
`
	if b.String() != want {
		t.Errorf("rendered:\n%s\nwant:\n%s", b.String(), want)
	}
}
