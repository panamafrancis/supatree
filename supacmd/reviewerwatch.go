package supacmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/panamafrancis/supatree/pkg/supatree"
	"github.com/panamafrancis/supatree/pkg/supatree/reviewer"
)

var reviewerWatchRaw bool

var reviewerWatchCmd = &cobra.Command{
	Use:   "watch [tree]",
	Short: "Follow the review running in a review tree",
	Long: "watch shows the reviewer's latest run in a review tree as it goes: what the\n" +
		"agent says, the tools it calls, and how the run ended. While the run is\n" +
		"going it keeps following and stops when the run ends; after that it shows the\n" +
		"last run's log. The review runs headless, as the tree's agent \"reviewer\", so\n" +
		"it is never in the tree's tab; `supatree open <tree> --agent=reviewer` resumes\n" +
		"that session once the run has ended. --raw prints the log as it is.",
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, err := resolveTreeName(args)
		if err != nil {
			return err
		}
		inst, err := supatree.Get(stCfg, name)
		if err != nil {
			return err
		}
		path, err := supatree.LatestHeadlessLog(inst.Root, reviewer.AgentName)
		if err != nil {
			return fmt.Errorf("%s: %w", inst.Name, err)
		}
		out := cmd.OutOrStdout()
		running := func() bool {
			_, ok := supatree.HeadlessRun(inst.Name, reviewer.AgentName)
			return ok
		}
		if running() {
			_, _ = fmt.Fprintf(out, "following %s (ctrl-c stops watching, not the review)\n\n", path)
		} else {
			_, _ = fmt.Fprintf(out, "no review is running in %s; its last run, %s:\n\n", inst.Name, path)
		}
		each := func(l string) { _, _ = fmt.Fprintln(out, l) }
		if !reviewerWatchRaw {
			each = func(l string) { renderStreamLine(out, l) }
		}
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return tailLines(ctx, path, true, each, func() bool { return !running() })
	},
}

// streamEvent is the part of a line of Claude's stream-json output that
// watch shows.
type streamEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Model   string `json:"model"`
	Message struct {
		Content []struct {
			Type    string          `json:"type"`
			Text    string          `json:"text"`
			Name    string          `json:"name"`
			Input   json.RawMessage `json:"input"`
			IsError bool            `json:"is_error"`
			Content json.RawMessage `json:"content"`
		} `json:"content"`
	} `json:"message"`
	Result     string  `json:"result"`
	IsError    bool    `json:"is_error"`
	DurationMS int64   `json:"duration_ms"`
	NumTurns   int     `json:"num_turns"`
	CostUSD    float64 `json:"total_cost_usd"`
}

// renderStreamLine writes one line of a headless run's log readably. Lines
// that are not stream-json — nono's, an error before the agent started — are
// written as they are; tool results are left out unless they failed.
func renderStreamLine(w io.Writer, line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	var ev streamEvent
	if !strings.HasPrefix(line, "{") || json.Unmarshal([]byte(line), &ev) != nil {
		_, _ = fmt.Fprintln(w, line)
		return
	}
	switch ev.Type {
	case "system":
		if ev.Subtype == "init" {
			_, _ = fmt.Fprintf(w, "── run started (%s)\n", orDash(ev.Model))
		}
	case "assistant":
		for _, c := range ev.Message.Content {
			switch c.Type {
			case "text":
				if t := strings.TrimSpace(c.Text); t != "" {
					_, _ = fmt.Fprintln(w, t)
				}
			case "tool_use":
				_, _ = fmt.Fprintf(w, "→ %s %s\n", toolName(c.Name), toolSummary(c.Input))
			}
		}
	case "user":
		for _, c := range ev.Message.Content {
			if c.Type == "tool_result" && c.IsError {
				_, _ = fmt.Fprintf(w, "  ✗ %s\n", firstLine(resultText(c.Content)))
			}
		}
	case "result":
		how := "finished"
		if ev.IsError || ev.Subtype != "success" {
			how = "ended: " + ev.Subtype
		}
		_, _ = fmt.Fprintf(w, "── run %s after %s, %d turns, $%.2f\n", how,
			(time.Duration(ev.DurationMS) * time.Millisecond).Round(time.Second), ev.NumTurns, ev.CostUSD)
	}
}

// toolName drops the mcp__<server>__ prefix: supatree's tools are what a
// review mostly calls, and the server name says nothing.
func toolName(name string) string {
	if rest, ok := strings.CutPrefix(name, "mcp__"); ok {
		if _, tool, ok := strings.Cut(rest, "__"); ok {
			return tool
		}
	}
	return name
}

// toolSummary is the input that says what a call does — a command, a path, a
// pattern — or the whole input, cut short.
func toolSummary(input json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	for _, k := range []string{"command", "file_path", "path", "pattern", "url", "pr"} {
		if v, ok := m[k].(string); ok && v != "" {
			return clip(firstLine(v), 120)
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		v, _ := json.Marshal(m[k])
		parts = append(parts, k+"="+string(v))
	}
	return clip(strings.Join(parts, " "), 120)
}

// resultText is a tool result's content, which is a string or a list of
// text blocks.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &blocks)
	var parts []string
	for _, b := range blocks {
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, " ")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func init() {
	reviewerWatchCmd.Flags().BoolVar(&reviewerWatchRaw, "raw", false, "print the log as it is, without rendering it")
	reviewerCmd.AddCommand(reviewerWatchCmd)
}
