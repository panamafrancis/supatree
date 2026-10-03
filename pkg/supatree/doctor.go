package supatree

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/panamafrancis/workbench/pkg/setup"
)

// Doctor checks what supatree needs to launch agents: zellij, git, nono, and
// every nono profile a configured model runs under — with each profile it
// extends, so a parent broken by a nono upgrade is named rather than found by
// an agent tab that closes the moment it opens. cfg may be nil (a config that
// does not load), in which case only supatree's own agent profile is checked.
func Doctor(cfg *Config) []setup.CheckResult {
	results := []setup.CheckResult{
		toolCheck("zellij", "brew install zellij", "--version"),
		toolCheck("git", "", "version"),
		toolCheck("nono", "install from https://nono.sh", "--version"),
	}
	if results[2].Status != setup.StatusOK {
		return results
	}
	profiles := map[string][]string{} // profile -> the models using it
	if cfg == nil {
		profiles[AgentProfileName] = nil
	} else {
		for key, m := range cfg.models() {
			if m.NonoProfile != "" {
				profiles[m.NonoProfile] = append(profiles[m.NonoProfile], key)
			}
		}
	}
	names := make([]string, 0, len(profiles))
	for p := range profiles {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		models := profiles[p]
		sort.Strings(models)
		label := "nono profile " + p
		if len(models) > 0 {
			label += " (model " + strings.Join(models, ", ") + ")"
		}
		results = append(results, profileCheck(label, p))
		seen := map[string]bool{p: true}
		for _, parent := range userProfileParents(p, seen) {
			results = append(results, profileCheck(fmt.Sprintf("nono profile %s, extended by %s", parent, p), parent))
		}
	}
	return results
}

func toolCheck(name, hint string, args ...string) setup.CheckResult {
	out, err := exec.CommandContext(context.Background(), name, args...).Output()
	if err != nil {
		if _, lookErr := exec.LookPath(name); lookErr != nil {
			return setup.CheckResult{Name: name, Status: setup.StatusFail, Message: "not found", Hint: hint}
		}
		return setup.CheckResult{Name: name, Status: setup.StatusFail, Message: err.Error(), Hint: hint}
	}
	return setup.CheckResult{Name: name, Status: setup.StatusOK, Message: strings.TrimSpace(string(out))}
}

func profileCheck(label, profile string) setup.CheckResult {
	if err := CheckNonoProfile(profile); err != nil {
		return setup.CheckResult{Name: label, Status: setup.StatusFail, Message: err.Error()}
	}
	return setup.CheckResult{Name: label, Status: setup.StatusOK, Message: "loads"}
}

// userProfileParents follows a user profile's extends chain through the
// profiles in nono's user directory, returning every parent it names. A
// built-in or package parent is returned but not followed: it has no user
// file, and `profile show` already resolves what it extends.
func userProfileParents(profile string, seen map[string]bool) []string {
	data, err := os.ReadFile(setup.NonoProfilePath(profile))
	if err != nil {
		return nil
	}
	var p struct {
		Extends json.RawMessage `json:"extends"`
	}
	if json.Unmarshal(data, &p) != nil || len(p.Extends) == 0 {
		return nil
	}
	var parents []string
	if json.Unmarshal(p.Extends, &parents) != nil {
		var one string
		if json.Unmarshal(p.Extends, &one) != nil || one == "" {
			return nil
		}
		parents = []string{one}
	}
	var out []string
	for _, parent := range parents {
		if parent == "" || seen[parent] {
			continue
		}
		seen[parent] = true
		out = append(out, parent)
		out = append(out, userProfileParents(parent, seen)...)
	}
	return out
}
