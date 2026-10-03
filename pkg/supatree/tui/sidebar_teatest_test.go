package tui

import (
	"testing"

	"github.com/charmbracelet/x/exp/teatest"
)

// Scenario 1: the default sidebar over the standard world. The golden is the
// whole screen; a diff in review is the visual change.
func TestSidebarDefaultScreen(t *testing.T) {
	s := startSidebar(t, sidebarOpts{})
	s.waitFor("paris")
	teatest.RequireEqualOutput(t, []byte(s.finish().View()))
}

// Scenarios 40-41: ? opens the reference, j scrolls it, and any other key
// closes it — through the running program, at the default width and at a
// narrower one where the entries fold.
func TestSidebarHelp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		width int
	}{{"w28", defaultSidebarWidth}, {"w20", 20}} {
		t.Run(tc.name, func(t *testing.T) {
			s := startSidebar(t, sidebarOpts{width: tc.width, height: 30})
			s.waitFor("paris")
			s.press("?")
			s.waitFor("Navigation")
			s.press("j")
			s.waitFor("j/k scroll (")
			m := s.finish()
			assertFits(t, "help", m.View(), tc.width)
			teatest.RequireEqualOutput(t, []byte(m.View()))
		})
	}
}

// Scenario 17: enter on an agent row opens that agent's tab. The fake zellij
// sees the tab created under the agent's name.
func TestSidebarEnterOpensAgentTab(t *testing.T) {
	s := startSidebar(t, sidebarOpts{inZellij: true})
	s.waitFor("reviewer")
	s.press("G", "k") // paris is the last tree; reviewer is above its repositories
	s.waitFor("enter open")
	s.press("enter")
	s.waitForZellij("paris:reviewer")
}
