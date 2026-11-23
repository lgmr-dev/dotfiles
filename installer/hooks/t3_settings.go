package hooks

import (
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/lgmr-dev/dotfiles/installer/platform"
	"github.com/lgmr-dev/dotfiles/installer/targets"
)

func (p *Profiles) t3Units() []targets.Desired {
	return profileFields(filepath.Join(p.Folders.Home, ".t3", "userdata", "settings.json"), []profileField{
		{"storageCleanup", `{"worktreeOnMerge":true,"worktreeOnDelete":true}`},
		{"enableProviderUpdateChecks", `false`},
		{"sidebarAutoSettleAfterDays", `null`},
		{"idleCompaction", `"onReturn"`},
		{"autoResumeLimitedThreads", `true`},
		{"sidebarAutoSettleOnMerge", `false`},
		{"defaultThreadEnvMode", `"worktree"`},
		{"branchNamingMode", `"semantic"`},
		{"removeAgentCreditsOnMerge", `true`},
		{"pullRequestMergeMethod", `"squash"`},
		{"defaultTheme", `"rose-pine-moon"`},
	})
}

func (p *Profiles) t3ThemeUnits() ([]targets.Desired, error) {
	data, err := platform.ReadDocument(filepath.Join(p.Checkout, "t3", "rose-pine-moon.json"))
	if err != nil {
		return nil, err
	}
	var theme map[string]json.RawMessage
	if err := json.Unmarshal(data, &theme); err != nil {
		return nil, err
	}
	names := []string{"version", "name", "appearance", "colors"}
	fields := []profileField{}
	for _, name := range names {
		value, ok := theme[name]
		if !ok {
			break
		}
		fields = append(fields, profileField{name, string(value)})
	}
	if len(fields) != len(names) || len(theme) != len(names) {
		return nil, errors.New("JSON recipe must declare exactly its managed fields")
	}
	return profileFields(filepath.Join(p.Folders.Home, ".t3", "userdata", "themes", "rose-pine-moon.json"), fields), nil
}
