package supatree

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/panamafrancis/workbench/pkg/config"
	"github.com/panamafrancis/workbench/pkg/zellij"
)

// DefaultPMName is the PM that exists before anyone adds another. It keeps the
// identity the single PM always had — tab supatree-pm, address st-pm, agent
// name "pm", requests.offset — so upgrading leaves a running PM where it was.
const DefaultPMName = "pm"

// PM is one product-manager agent. Every PM runs in PMDir(): they share its
// instructions, grants and queues, and resume independently via SessionID —
// the same arrangement as several agents sharing a supatree root.
//
// Order matters. The first PM is the *top* PM: it is what a cold start opens,
// what P and m reach, and the reader of every request not addressed to a PM
// by name.
type PM struct {
	Name      string    `yaml:"name"`
	Model     string    `yaml:"model,omitempty"`
	SessionID string    `yaml:"session_id,omitempty"`
	CreatedAt time.Time `yaml:"created_at"`
}

func (p PM) isDefault() bool { return p.Name == DefaultPMName }

// AgentID is the PM's name as an agent: what SUPATREE_AGENT says, what its mail
// is from, and what a tree agent passes to message_agent to reach this PM in
// particular. Agent names cannot hold a colon, hence the dash.
func (p PM) AgentID() string {
	if p.isDefault() {
		return PMAgentName
	}
	return PMAgentName + "-" + p.Name
}

// Tab is the Zellij tab this PM opens into, under the reserved PMTab prefix.
func (p PM) Tab() string {
	if p.isDefault() {
		return PMTab
	}
	return PMTab + ":" + p.Name
}

// Address is the PM's name on the message bus.
func (p PM) Address() string {
	if p.isDefault() {
		return PMAddress
	}
	return PMAddress + "-" + p.Name
}

// OffsetPath records how far this PM has read into the request queue. Each PM
// has its own, so one PM reading the queue never marks another's requests read.
func (p PM) OffsetPath() string {
	if p.isDefault() {
		return PMOffsetPath()
	}
	return filepath.Join(PMDir(), "requests."+p.Name+".offset")
}

// ModelKey is the model entry this PM runs under: its own, else the PM default.
func (p PM) ModelKey(cfg *Config) string {
	if p.Model != "" {
		return p.Model
	}
	return cfg.PMModel()
}

// PMsPath is the PM registry, in the PMs' shared home.
func PMsPath() string {
	return filepath.Join(PMDir(), "pms.yml")
}

func pmsLockPath() string {
	return PMsPath() + ".lock"
}

type pmsFile struct {
	PMs []PM `yaml:"pms"`
}

// LoadPMs returns every PM, top first. There is always at least one: a missing
// or empty registry is the install from before PMs were plural, and reads as
// the default PM alone, pinned to the conversation it already had.
func LoadPMs() ([]PM, error) {
	pms, err := readPMs()
	if err != nil {
		return nil, err
	}
	if len(pms) > 0 {
		return pms, nil
	}
	var out []PM
	err = config.WithFileLock(pmsLockPath(), func() error {
		// Re-read under the lock: a concurrent caller may have just migrated.
		got, err := readPMs()
		if err != nil {
			return err
		}
		if len(got) > 0 {
			out = got
			return nil
		}
		def, err := defaultPM()
		if err != nil {
			return err
		}
		out = []PM{def}
		return savePMs(out)
	})
	return out, err
}

// defaultPM is the PM an existing install already has. Its session is pinned
// to the newest transcript in the PM's home, which is exactly the one the old
// directory-scoped --continue would have resumed; pinning it now is what stops
// a second PM's conversation from becoming "the newest" and being resumed in
// its place.
func defaultPM() (PM, error) {
	sid := latestTranscriptID(PMDir())
	if sid == "" {
		var err error
		if sid, err = newSessionID(); err != nil {
			return PM{}, err
		}
	}
	return PM{Name: DefaultPMName, SessionID: sid, CreatedAt: time.Now().UTC()}, nil
}

func readPMs() ([]PM, error) {
	data, err := os.ReadFile(PMsPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read PMs: %w", err)
	}
	var f pmsFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse PMs: %w", err)
	}
	return f.PMs, nil
}

func savePMs(pms []PM) error {
	if err := os.MkdirAll(PMDir(), 0755); err != nil {
		return fmt.Errorf("create PM dir: %w", err)
	}
	data, err := yaml.Marshal(pmsFile{PMs: pms})
	if err != nil {
		return fmt.Errorf("marshal PMs: %w", err)
	}
	return writeFileAtomic(PMsPath(), data)
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", filepath.Base(path), err)
	}
	return nil
}

// FindPM returns the PM named name, or nil.
func FindPM(pms []PM, name string) *PM {
	for i := range pms {
		if pms[i].Name == name {
			return &pms[i]
		}
	}
	return nil
}

// ResolvePM returns the PM named name, or the top PM when name is empty.
func ResolvePM(name string) (PM, error) {
	pms, err := LoadPMs()
	if err != nil {
		return PM{}, err
	}
	if name == "" {
		return pms[0], nil
	}
	if p := FindPM(pms, name); p != nil {
		return *p, nil
	}
	return PM{}, fmt.Errorf("no PM named %q — `supatree pm ls` lists them", name)
}

// IsPMAgentID reports whether an agent name has the shape of a PM's, without
// consulting the registry — a sandboxed tree agent cannot read it. Mail to a
// PM that no longer exists still reaches the top PM, so the shape is enough.
func IsPMAgentID(id string) bool {
	return id == PMAgentName || strings.HasPrefix(id, PMAgentName+"-")
}

// ValidatePMName rejects a name a new PM cannot take. DefaultPMName is
// reserved: identity follows the name, not the position, so a re-added "pm"
// below the top would answer to the top PM's agent name, tab and offset, and
// its mail would be forwarded to the top PM instead.
func ValidatePMName(name string) error {
	if err := ValidateAgentName(name); err != nil {
		return fmt.Errorf("invalid PM name: %w", err)
	}
	if name == DefaultPMName {
		return fmt.Errorf("%q is reserved for the default PM", name)
	}
	return nil
}

// AddPM registers a new PM at the bottom of the list. Its read position starts
// at the end of the request queue: requests raised before it existed were never
// addressed to it.
func AddPM(name, model string) (PM, error) {
	if err := ValidatePMName(name); err != nil {
		return PM{}, err
	}
	if _, err := LoadPMs(); err != nil { // migrate first, so the default PM stays on top
		return PM{}, err
	}
	sid, err := newSessionID()
	if err != nil {
		return PM{}, err
	}
	p := PM{Name: name, Model: model, SessionID: sid, CreatedAt: time.Now().UTC()}
	err = config.WithFileLock(pmsLockPath(), func() error {
		pms, err := readPMs()
		if err != nil {
			return err
		}
		if FindPM(pms, name) != nil {
			return fmt.Errorf("a PM named %q already exists", name)
		}
		if err := startOffsetAtEnd(p); err != nil {
			return err
		}
		return savePMs(append(pms, p))
	})
	if err != nil {
		return PM{}, err
	}
	return p, nil
}

// RemovePM drops a PM from the registry, with its read position and mailbox.
// Its transcript stays: it sits in the directory every PM shares, which cannot
// be archived wholesale without taking the others' with it.
//
// The last PM cannot be removed — requests nobody addressed would pile up with
// no reader. Removing the top PM promotes the next, which takes over the
// unaddressed requests from wherever the old top had read to.
func RemovePM(name string) (PM, error) {
	if _, err := LoadPMs(); err != nil {
		return PM{}, err
	}
	var removed PM
	err := config.WithFileLock(pmsLockPath(), func() error {
		pms, err := readPMs()
		if err != nil {
			return err
		}
		i := -1
		for j := range pms {
			if pms[j].Name == name {
				i = j
			}
		}
		if i < 0 {
			return fmt.Errorf("no PM named %q", name)
		}
		if len(pms) == 1 {
			return fmt.Errorf("%q is the only PM; add another before removing it", name)
		}
		removed = pms[i]
		rest := append(append([]PM{}, pms[:i]...), pms[i+1:]...)
		if i == 0 {
			handOverOffset(removed, rest[0])
		}
		if err := savePMs(rest); err != nil {
			return err
		}
		_ = os.Remove(removed.OffsetPath())
		_ = os.RemoveAll(MailDir(PMDir(), removed.AgentID()))
		return nil
	})
	return removed, err
}

// handOverOffset moves the new top PM's read position back to the old top's
// when that is earlier, so the unaddressed requests the old top never read are
// not skipped. Re-reading a few of its own is the cheaper failure.
func handOverOffset(from, to PM) {
	old := loadOffsetAt(from.OffsetPath())
	cur := loadOffsetAt(to.OffsetPath())
	if old.Offset < cur.Offset {
		_ = writeJSONAtomic(to.OffsetPath(), old)
	}
}

// ClosePMTab closes a PM's tab in every supatree session. Best effort, like
// closing a removed tree's tabs: the registry is the source of truth, and a
// stray tab is closable by hand.
func ClosePMTab(ws zellij.Workspace, p PM) []string {
	sessions, err := zellij.ListSessions()
	if err != nil {
		return []string{fmt.Sprintf("could not list zellij sessions to close %s's tab: %v", p.Name, err)}
	}
	tab := p.Tab()
	var warnings []string
	for _, s := range sessions {
		if s.Exited || !strings.HasPrefix(s.Name, ws.SessionPrefix) {
			continue
		}
		if _, err := zellij.CloseTabsIn(s.Name, func(name string) bool { return name == tab }); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", s.Name, err))
		}
	}
	ws.CleanupLayout(tab)
	return warnings
}

// coldStartPath marks a session `supatree start` has just created, for its
// first sidebar to open the top PM in.
func coldStartPath(session string) string {
	return filepath.Join(StateRoot(), "coldstart", strings.ReplaceAll(session, string(filepath.Separator), "_"))
}

// MarkColdStart records that session is being created rather than attached to.
func MarkColdStart(session string) error {
	path := coldStartPath(session)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, nil, 0644)
}

// TakeColdStart consumes session's cold-start marker, reporting whether there
// was one. Removal is the claim: of several callers, exactly one gets true.
func TakeColdStart(session string) bool {
	if session == "" {
		return false
	}
	return os.Remove(coldStartPath(session)) == nil
}

// latestTranscriptID returns the session ID of the newest Claude transcript
// recorded for dir, or "" if there is none.
func latestTranscriptID(dir string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	entries, err := os.ReadDir(filepath.Join(home, ".claude", "projects", encodeProjectPath(dir)))
	if err != nil {
		return ""
	}
	var best string
	var bestAt time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if best == "" || info.ModTime().After(bestAt) {
			best, bestAt = strings.TrimSuffix(e.Name(), ".jsonl"), info.ModTime()
		}
	}
	return best
}

// encodeProjectPath is how Claude names a project's transcript directory: every
// character outside [A-Za-z0-9] becomes a dash. It mirrors the unexported
// helper in workbench's sandbox package.
func encodeProjectPath(p string) string {
	var b strings.Builder
	for _, r := range p {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}
