package supatree

import (
	"testing"

	"github.com/panamafrancis/workbench/pkg/zellij"
)

// agentExecCmd is a supatree agent pane's command as zellij 0.44 lists it: the
// nono shim has exec'd agent-exec, so nono is not the first word.
const agentExecCmd = "/Users/me/go/bin/supatree agent-exec --nono /opt/homebrew/bin/nono --shim-dir /c/shim -- run --profile p -- claude --resume x --name st-pm"

func TestPickAgentPane(t *testing.T) {
	panes := []zellij.Pane{
		{ID: 0, TabName: PMTab, Title: "sidebar", Command: "bash -c while true; do supatree ls; done"},
		{ID: 1, TabName: PMTab, Title: PMTab, Command: agentExecCmd},
		{ID: 2, TabName: "accra", Title: "accra", Command: "/opt/homebrew/bin/nono run --profile p -- claude"},
		{ID: 3, TabName: "lima", Title: "✳ Fixing bugs", Command: agentExecCmd},
		{ID: 4, TabName: "wb", Title: "✳ Fixing bugs", Command: agentExecCmd},
		{ID: 5, TabName: "wb", Title: "~/trees/repos/wb", Command: "/bin/zsh"},
		{ID: 6, TabName: "oslo", Title: "oslo", Command: "/bin/zsh"},
		{ID: 7, TabName: "rome", Title: "rome", Command: agentExecCmd, Exited: true},
		{ID: 8, TabName: "kiev", Title: "kiev", Command: ""},
		{ID: 9, TabName: "two", Title: "a", Command: agentExecCmd},
		{ID: 10, TabName: "two", Title: "b", Command: agentExecCmd},
		{ID: 11, TabName: "paris", Title: "sidebar", Command: agentExecCmd},
	}
	for _, tc := range []struct {
		tab    string
		want   int
		wantOK bool
	}{
		{PMTab, 1, true},    // behind agent-exec, titled after the tab
		{"accra", 2, true},  // nono directly
		{"lima", 3, true},   // retitled, but the only agent
		{"wb", 4, true},     // a path-titled shell never wins
		{"oslo", 0, false},  // a shell titled after the tab is not an agent
		{"rome", 0, false},  // exited
		{"kiev", 8, true},   // no command listed: a lone title match counts
		{"two", 0, false},   // two untitled agents: not sure which
		{"paris", 0, false}, // the sidebar is never the agent
	} {
		p, ok := pickAgentPane(panes, tc.tab)
		if ok != tc.wantOK || (ok && p.ID != tc.want) {
			t.Errorf("%s: got pane %d, %v; want %d, %v", tc.tab, p.ID, ok, tc.want, tc.wantOK)
		}
	}
}
