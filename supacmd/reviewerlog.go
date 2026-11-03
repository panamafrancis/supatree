package supacmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

var (
	reviewerLogFollow bool
	reviewerLogLines  int
)

var reviewerLogCmd = &cobra.Command{
	Use:   "log",
	Short: "Show the reviewer's lines from the watcher's log",
	Long: "log prints the lines of the watcher's log (watch.log) that are about the\n" +
		"reviewer: trees created and removed, runs started and finished, verdicts\n" +
		"confirmed on GitHub, failures, and a summary whenever it changes. -f keeps\n" +
		"following it. A --headless watcher logs to stderr instead, so there is no\n" +
		"file to read.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		path := watchLogPath()
		var lines []string
		for _, p := range []string{path + ".1", path} {
			got, err := reviewerLines(p)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			lines = append(lines, got...)
		}
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) && !reviewerLogFollow {
			return fmt.Errorf("no watcher log at %s (a --headless watcher logs to stderr)", path)
		}
		if reviewerLogLines > 0 && len(lines) > reviewerLogLines {
			lines = lines[len(lines)-reviewerLogLines:]
		}
		out := cmd.OutOrStdout()
		for _, l := range lines {
			_, _ = fmt.Fprintln(out, l)
		}
		if !reviewerLogFollow {
			return nil
		}
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return followReviewerLog(ctx, path, out)
	},
}

// isReviewerLine reports whether a watcher log line is the reviewer's: its
// own lines start "reviewer: " after the timestamp. Matching the word alone
// would take in every agent someone named reviewer.
func isReviewerLine(line string) bool {
	_, msg, ok := strings.Cut(line, "] ")
	return ok && (strings.HasPrefix(msg, "reviewer: ") || strings.HasSuffix(msg, "staying up for the reviewer"))
}

func reviewerLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		if isReviewerLine(sc.Text()) {
			out = append(out, sc.Text())
		}
	}
	return out, sc.Err()
}

// followReviewerLog prints reviewer lines as the watcher appends them,
// starting at the log's current end.
func followReviewerLog(ctx context.Context, path string, out io.Writer) error {
	return tailLines(ctx, path, false, func(l string) {
		if isReviewerLine(l) {
			_, _ = fmt.Fprintln(out, l)
		}
	}, nil)
}

// tailLines calls each with every complete line appended to path, from its
// start or from its current end, until ctx is done or done reports true —
// checked once a second, after what was written by then has been read. The
// watcher rotates its log by renaming it, so a file that shrank or was
// replaced is read again from the start.
func tailLines(ctx context.Context, path string, fromStart bool, each func(string), done func() bool) error {
	var offset int64
	var ident os.FileInfo
	if st, err := os.Stat(path); err == nil && !fromStart {
		offset, ident = st.Size(), st
	} else if err == nil {
		ident = st
	}
	var partial string
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for first := true; ; first = false {
		finished := done != nil && done()
		if !first {
			select {
			case <-ctx.Done():
				return nil
			case <-tick.C:
			}
		}
		if st, err := os.Stat(path); err == nil { // else between a rotation's rename and the new file
			if ident == nil || !os.SameFile(ident, st) || st.Size() < offset {
				offset, partial = 0, ""
			}
			ident = st
			if st.Size() > offset {
				data, err := readFrom(path, offset)
				if err == nil {
					offset += int64(len(data))
					lines := strings.Split(partial+string(data), "\n")
					partial = lines[len(lines)-1] // an unfinished line waits for its end
					for _, l := range lines[:len(lines)-1] {
						each(l)
					}
				}
			}
		}
		if finished {
			if partial != "" {
				each(partial)
			}
			return nil
		}
	}
}

func readFrom(path string, offset int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}

func init() {
	reviewerLogCmd.Flags().BoolVarP(&reviewerLogFollow, "follow", "f", false, "keep printing new lines as the watcher writes them")
	reviewerLogCmd.Flags().IntVarP(&reviewerLogLines, "lines", "n", 50, "how many earlier lines to show (0: all)")
	reviewerCmd.AddCommand(reviewerLogCmd)
}
