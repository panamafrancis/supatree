package supatree

import (
	"path/filepath"
	"strings"

	"github.com/panamafrancis/workbench/pkg/zellij"
)

// sidebarPaneName is the title the shared tab layout gives a tab's sidebar.
const sidebarPaneName = "sidebar"

// AgentPane finds the running agent pane of the tab named tab, for typing
// into it. It is zellij.AgentPane's rule with one difference: a supatree agent
// pane's foreground command is not nono but `supatree agent-exec --nono …`,
// because the pane's PATH puts the agent-exec shim in front of nono
// (withAgentExec). zellij.AgentPane only recognises nono itself, so it never
// found a supatree agent, and both the wake (#15) and the PM nudge skipped
// every one as uncertain.
func AgentPane(session, tab string) (zellij.Pane, bool, error) {
	panes, err := zellij.ListPanes(session)
	if err != nil {
		return zellij.Pane{}, false, err
	}
	p, ok := pickAgentPane(panes, tab)
	return p, ok, nil
}

// pickAgentPane chooses among the panes running an agent: the one titled after
// the tab (or "<alias>/<tab>") wins, else — since an agent may retitle its own
// terminal — the only one. A title alone is not enough while the pane's command
// is known to be something else: a user's shell rooted in the worktree is
// commonly titled with a path ending in the tab's name, and the caller is about
// to type into the match. Only when the listing carries no command at all does
// a lone title match count.
func pickAgentPane(panes []zellij.Pane, tab string) (zellij.Pane, bool) {
	var agents, titledUnknown []zellij.Pane
	for _, p := range panes {
		if p.TabName != tab || p.IsPlugin || p.Exited || p.IsHeld || p.Title == sidebarPaneName {
			continue
		}
		titled := p.Title == tab || strings.HasSuffix(p.Title, "/"+tab)
		switch {
		case runsAgent(p.Command):
			if titled {
				return p, true
			}
			agents = append(agents, p)
		case titled && strings.TrimSpace(p.Command) == "":
			titledUnknown = append(titledUnknown, p)
		}
	}
	if len(agents) == 1 {
		return agents[0], true
	}
	if len(agents) == 0 && len(titledUnknown) == 1 {
		return titledUnknown[0], true
	}
	return zellij.Pane{}, false
}

// runsAgent reports whether a pane's foreground command is an agent's sandbox:
// nono itself, or nono behind supatree's agent-exec shim.
func runsAgent(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	if filepath.Base(fields[0]) == "nono" {
		return true
	}
	return len(fields) > 1 && fields[1] == "agent-exec"
}
