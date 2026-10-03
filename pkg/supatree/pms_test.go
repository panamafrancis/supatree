package supatree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/panamafrancis/workbench/pkg/testutil"
)

// An install from before PMs were plural has one PM, and it must come back as
// the same agent: same tab, address and offset, and resuming the conversation
// --continue would have picked — the newest transcript in the PM's home.
func TestLoadPMsMigratesTheSinglePM(t *testing.T) {
	testutil.IsolateHome(t)
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".claude", "projects", encodeProjectPath(PMDir()))
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	old, newest := filepath.Join(dir, "old.jsonl"), filepath.Join(dir, "newest.jsonl")
	for _, p := range []string{old, newest} {
		if err := os.WriteFile(p, []byte("{}\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	pms, err := LoadPMs()
	if err != nil {
		t.Fatal(err)
	}
	if len(pms) != 1 {
		t.Fatalf("pms = %+v, want the default PM alone", pms)
	}
	p := pms[0]
	if p.Name != DefaultPMName || p.Tab() != PMTab || p.Address() != PMAddress || p.AgentID() != PMAgentName || p.OffsetPath() != PMOffsetPath() {
		t.Errorf("default PM = %+v (tab %s, address %s), want the old identity", p, p.Tab(), p.Address())
	}
	if p.SessionID != "newest" {
		t.Errorf("session = %q, want the newest transcript pinned", p.SessionID)
	}
	// Persisted, so the pin does not drift once a second PM writes transcripts.
	if again, _ := LoadPMs(); len(again) != 1 || again[0].SessionID != "newest" {
		t.Errorf("reload = %+v, want the same pinned PM", again)
	}
}

func TestAddAndRemovePMs(t *testing.T) {
	testutil.IsolateHome(t)
	p, err := AddPM("research", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Tab() != PMTab+":research" || p.Address() != PMAddress+"-research" || p.AgentID() != "pm-research" || p.SessionID == "" {
		t.Errorf("new PM = %+v, want its own tab, address, agent name and session", p)
	}
	pms, _ := LoadPMs()
	if len(pms) != 2 || pms[0].Name != DefaultPMName || pms[1].Name != "research" {
		t.Fatalf("pms = %+v, want the default on top and the new one below", pms)
	}
	if pms[0].SessionID == pms[1].SessionID {
		t.Error("two PMs share a session — they would resume one conversation")
	}
	if _, err := AddPM("research", ""); err == nil {
		t.Error("a duplicate PM name was accepted")
	}
	if _, err := AddPM("../x", ""); err == nil {
		t.Error("a PM name that is not a path segment was accepted")
	}

	if _, err := RemovePM(DefaultPMName); err != nil {
		t.Fatal(err)
	}
	if top, _ := ResolvePM(""); top.Name != "research" {
		t.Errorf("top after removing the default = %q, want research promoted", top.Name)
	}
	if _, err := RemovePM("research"); err == nil || !strings.Contains(err.Error(), "only PM") {
		t.Errorf("removing the last PM: err = %v, want a refusal", err)
	}
	if _, err := RemovePM("nobody"); err == nil {
		t.Error("removing an unknown PM succeeded")
	}
}

// Each PM reads its own requests; unaddressed ones — and those for a PM that is
// gone — go to the top PM alone, so two PMs never both act on one.
func TestRequestsRouteToOnePM(t *testing.T) {
	testutil.IsolateHome(t)
	if err := AppendRequest(Request{From: fromCLI, Text: "before research existed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := AddPM("research", ""); err != nil {
		t.Fatal(err)
	}
	for _, r := range []Request{
		{From: fromCLI, Text: "for anyone"},
		{From: fromCLI, To: "research", Text: "for research"},
		{From: fromCLI, To: "gone", Text: "for a removed PM"},
	} {
		if err := AppendRequest(r); err != nil {
			t.Fatal(err)
		}
	}

	top, _, _ := PendingRequests("")
	if got := texts(top); got != "before research existed|for anyone|for a removed PM" {
		t.Errorf("top PM sees %q", got)
	}
	res, off, _ := PendingRequests("research")
	if got := texts(res); got != "for research" {
		t.Errorf("research sees %q, want only its own (and nothing from before it existed)", got)
	}
	if _, _, err := PendingRequests("nobody"); err == nil {
		t.Error("an unknown PM read the queue")
	}

	// Research reading its own does not mark the top PM's read.
	if err := CommitRequests("research", off); err != nil {
		t.Fatal(err)
	}
	if again, _, _ := PendingRequests(""); len(again) != 3 {
		t.Errorf("top PM lost requests to another PM's commit: %q", texts(again))
	}

	// Removing the top PM hands its unread unaddressed requests to the next.
	if _, err := RemovePM(DefaultPMName); err != nil {
		t.Fatal(err)
	}
	promoted, _, _ := PendingRequests("")
	if !strings.Contains(texts(promoted), "for anyone") {
		t.Errorf("promoted PM sees %q, want the old top's backlog", texts(promoted))
	}
}

const fromCLI = "cli"

func texts(reqs []Request) string {
	out := make([]string, len(reqs))
	for i, r := range reqs {
		out[i] = r.Text
	}
	return strings.Join(out, "|")
}

// Mail a tree agent leaves for "pm-<name>" reaches that PM; plain "pm" reaches
// the top PM, as it always did.
func TestForwardPMMailAddressesThePM(t *testing.T) {
	testutil.IsolateHome(t)
	if _, err := AddPM("research", ""); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	inst := &Instance{Name: treeA, Root: root}
	if err := Deliver(root, "pm-research", "main", "done with the spike"); err != nil {
		t.Fatal(err)
	}
	if err := Deliver(root, PMAgentName, "main", "blocked on review"); err != nil {
		t.Fatal(err)
	}
	if n, err := ForwardPMMail([]*Instance{inst}); err != nil || n != 2 {
		t.Fatalf("ForwardPMMail = %d, %v; want 2, nil", n, err)
	}
	if res, _, _ := PendingRequests("research"); texts(res) != "done with the spike" {
		t.Errorf("research sees %q", texts(res))
	}
	if top, _, _ := PendingRequests(""); texts(top) != "blocked on review" {
		t.Errorf("top PM sees %q", texts(top))
	}
}

// The marker is taken exactly once, so only the first sidebar of a new session
// opens the PM.
func TestColdStartMarkerIsTakenOnce(t *testing.T) {
	testutil.IsolateHome(t)
	if TakeColdStart("st-main") {
		t.Fatal("took a marker nobody set")
	}
	if err := MarkColdStart("st-main"); err != nil {
		t.Fatal(err)
	}
	if !TakeColdStart("st-main") {
		t.Fatal("marker not taken")
	}
	if TakeColdStart("st-main") {
		t.Error("marker taken twice — every sidebar would open the PM")
	}
	if TakeColdStart("") {
		t.Error("an empty session took a marker")
	}
}
