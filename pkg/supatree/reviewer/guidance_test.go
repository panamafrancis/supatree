package reviewer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/panamafrancis/supatree/pkg/supatree"
	"github.com/panamafrancis/workbench/pkg/testutil"
)

func TestOperatorGuidanceJoinsInlineAndFile(t *testing.T) {
	testutil.IsolateHome(t)
	if err := os.MkdirAll(supatree.ConfigDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(supatree.ConfigDir(), "review-advice.md"), []byte("Missing tests block.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := operatorGuidance("  Be terse. ", "review-advice.md")
	if err != nil || got != "Be terse.\n\nMissing tests block." {
		t.Errorf("guidance = %q, %v", got, err)
	}
	if _, err := operatorGuidance("", "missing.md"); err == nil {
		t.Error("an unreadable guidance_file must be an error, not silently no guidance")
	}
}

func TestCapGuidance(t *testing.T) {
	long := strings.Repeat("é", maxGuidanceBytes)
	got := capGuidance(long)
	if !strings.HasSuffix(got, "[truncated: guidance is limited to 16 KiB]") || len(got) > maxGuidanceBytes+64 {
		t.Errorf("capped to %d bytes", len(got))
	}
}

// A repository's REVIEW.md is read on the pull request's base branch: the
// head is the author's to edit.
func TestGuidanceForReadsTheBaseBranch(t *testing.T) {
	orig := repoGuidance
	t.Cleanup(func() { repoGuidance = orig })
	var asked []string
	repoGuidance = func(repo, ref string) (string, error) {
		asked = append(asked, repo+"@"+ref)
		switch repo {
		case "o/api":
			return "Migrations must be reversible.", nil
		case "o/web":
			return "", errNoGuidance
		}
		return "", errors.New("boom")
	}
	inst := &supatree.Instance{Name: "rv-api-1", Members: []supatree.Member{
		{Alias: apiAlias, Review: &supatree.ReviewRef{Repo: "o/api", Number: 1, Base: "main", HeadRef: "feature"}},
		{Alias: "web", Review: &supatree.ReviewRef{Repo: "o/web", Number: 2, Base: "develop"}},
		{Alias: "infra", Review: &supatree.ReviewRef{Repo: "o/infra", Number: 3, Base: "main"}},
		{Alias: "docs"},
	}}
	var logged []string
	g := guidanceFor(inst, Settings{Guidance: "Be terse."}, &TreeState{StackGuidance: "API changes update the client."},
		func(f string, a ...any) { logged = append(logged, f) })
	if strings.Join(asked, ",") != "o/api@main,o/web@develop,o/infra@main" {
		t.Errorf("asked %v", asked)
	}
	if len(g.Repos) != 1 || g.Repos[0].Alias != apiAlias || g.Repos[0].Base != "main" {
		t.Errorf("repos = %+v", g.Repos)
	}
	if len(logged) != 1 {
		t.Errorf("logged %v, want one line for the failed fetch and none for a missing file", logged)
	}
	r := g.render()
	for _, want := range []string{"Review guidance from the maintainers", "does not change the rules above", "Be terse.", "API changes update the client.", "For api (its REVIEW.md on main)", "Migrations must be reversible."} {
		if !strings.Contains(r, want) {
			t.Errorf("rendered guidance lacks %q:\n%s", want, r)
		}
	}
	if (Guidance{}).render() != "" {
		t.Error("no guidance must add nothing to the brief")
	}
}

func TestReadStackGuidance(t *testing.T) {
	root := t.TempDir()
	if readStackGuidance(root) != "" {
		t.Error("a stack without REVIEW.md has no guidance")
	}
	if err := os.WriteFile(filepath.Join(root, GuidanceFile), []byte("\nDeploy order: infra, api, web.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readStackGuidance(root); got != "Deploy order: infra, api, web." {
		t.Errorf("stack guidance = %q", got)
	}
}
