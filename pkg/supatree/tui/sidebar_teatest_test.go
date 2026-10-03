package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"
)

// Scenario 1: the default sidebar over the standard world. The golden is the
// whole screen; a diff in review is the visual change.
func TestSidebarDefaultScreen(t *testing.T) {
	s := startSidebar(t, sidebarOpts{})
	s.waitFor(paris)
	teatest.RequireEqualOutput(t, []byte(s.finish().View()))
}

// Scenarios 40-41: ? opens the reference and j scrolls it, through the
// running program — at the default width, at 20 where descriptions hang
// beside their keys, and at 16 where they fold under them.
func TestSidebarHelp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		width int
	}{{"w28", defaultSidebarWidth}, {"w20", 20}, {"w16", 16}} {
		t.Run(tc.name, func(t *testing.T) {
			s := startSidebar(t, sidebarOpts{width: tc.width, height: 30})
			s.waitFor(paris)
			s.press("?", "j")
			m := s.finish()
			if m.mode != modeHelp || m.helpScroll != 1 {
				t.Fatalf("mode %v, scroll %d; want the help open, scrolled one line", m.mode, m.helpScroll)
			}
			assertFits(t, "help", m.View(), tc.width)
			teatest.RequireEqualOutput(t, []byte(m.View()))
		})
	}
}

// Pressing q in the sidebar asks first, and y quits the program.
func TestSidebarQuitConfirms(t *testing.T) {
	s := startSidebar(t, sidebarOpts{})
	s.waitFor(paris)
	s.press("q")
	s.waitFor("quit sidebar?")
	s.press("y")
	s.tm.WaitFinished(t, teatest.WithFinalTimeout(5*time.Second))
}

// Scenario 17: enter on an agent row opens that agent's tab. The fake zellij
// sees the tab created under the agent's name.
func TestSidebarEnterOpensAgentTab(t *testing.T) {
	s := startSidebar(t, sidebarOpts{inZellij: true})
	s.waitFor("reviewer")
	s.press("G", "k") // paris is the last tree; reviewer is above its repositories
	s.waitFor("enter open")
	s.press(tea.KeyEnter)
	s.waitForZellij("paris:reviewer")
}
