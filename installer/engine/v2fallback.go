package engine

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/state"
)

const switchingLine = "The import from release v2 is not complete. Run setup to complete it, or run restore-v2 to go back to release v2."

// V2Folders are the folders in the state folder that release v2 wrote and
// that the import leaves in place, and the restore set of the import. Setup
// never deletes them.
var V2Folders = []string{"packages", "nvim-sync", "configuration", "profiles", "recovery", "v2-restore"}

// FallbackNotice returns the line that a preview shows for the v2_restore
// value of its plan. While the fallback is open, a confirmed apply or update
// closes it before the first change. It is empty for a check and for a state
// without a fallback.
func FallbackNotice(plan Plan) string {
	if plan.Mode != "apply" && plan.Mode != "update" {
		return ""
	}
	switch plan.V2Restore {
	case "open":
		return "This closes the v2 fallback: after you confirm this run, restore-v2 is not available. The v2 folders stay for your cleanup."
	case "switching":
		return switchingLine
	}
	return ""
}

func within(folder, path string) bool {
	relative, err := filepath.Rel(folder, path)
	return err == nil && filepath.IsAbs(path) && (relative == "." || filepath.IsLocal(relative))
}

func v2References(st state.State) []string {
	paths := []string{}
	for key, record := range st.Targets {
		for _, value := range []string{record.Applied, record.Next, strings.TrimPrefix(record.Before, "backup:")} {
			if filepath.IsAbs(value) {
				paths = append(paths, value)
			}
		}
		if record.Kind == "link" {
			if target, err := os.Readlink(key); err == nil {
				paths = append(paths, target)
			}
		}
	}
	return paths
}

func (e *Engine) v2FolderReport(fallback string) string {
	present := []string{}
	for _, name := range V2Folders {
		folder := filepath.Join(e.StateDir, name)
		if _, err := os.Lstat(folder); err == nil {
			present = append(present, folder)
		}
	}
	if len(present) == 0 || fallback == "switching" {
		return ""
	}
	if fallback == "open" {
		return "\nNote: the v2 fallback is open. Keep these folders of release v2 while restore-v2 can use them: " + strings.Join(present, ", ")
	}
	st, err := state.Peek(e.StateDir, e.Target)
	if err != nil {
		return "\nNote: setup could not examine the folders of release v2: " + err.Error()
	}
	references := v2References(st)
	message := ""
	for _, folder := range present {
		if slices.ContainsFunc(references, func(path string) bool { return within(folder, path) }) {
			message += "\nNote: " + folder + " is a folder of release v2 that a recorded target or baseline still uses; keep it"
			continue
		}
		message += "\nOwner cleanup: " + folder + " is a folder of release v2 that setup no longer uses; inspect it, then delete it yourself"
	}
	return message + "\nNote: the reference check of the v2 folders covers the recorded link targets and baselines only. A file that you changed can still name a path in these folders."
}
