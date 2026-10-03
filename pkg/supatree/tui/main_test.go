package tui

import (
	"os"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/panamafrancis/workbench/pkg/testutil"
)

// TestMain points HOME and the XDG dirs at a throwaway directory for the whole
// binary, so a test that forgets to isolate itself still cannot write into the
// real ~/.config, ~/.local/state or ~/.cache.
//
// It also renders without colour, so a golden file holds the screen's text
// rather than escape codes that vary with the terminal running the tests.
func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.Ascii)
	cleanup := testutil.IsolateProcess()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
