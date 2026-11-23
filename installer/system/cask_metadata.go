package system

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

type caskMetadata struct {
	Token       string                       `json:"token"`
	FullToken   string                       `json:"full_token"`
	Tap         string                       `json:"tap"`
	SHA256      string                       `json:"sha256"`
	AutoUpdates bool                         `json:"auto_updates"`
	DependsOn   map[string]json.RawMessage   `json:"depends_on"`
	Artifacts   []map[string]json.RawMessage `json:"artifacts"`
}

// ValidateCaskMetadata checks one cask document of `brew info --json=v2`
// against the reviewed recipe: the homebrew/cask tap, and only the reviewed
// artifacts at their reviewed paths. For a cask that setup can install
// (installed is false) it also requires a SHA-256, automatic updates and no
// dependency except the macOS requirement. With installed, data is the
// recipe that Homebrew saved for removal.
func ValidateCaskMetadata(data []byte, s CaskSpec, installed bool) error {
	var doc caskMetadata
	if len(data) > 1<<20 || json.Unmarshal(data, &doc) != nil || doc.Token != s.Token || doc.FullToken != s.Token || doc.Tap != "homebrew/cask" {
		return errors.New("native app metadata has an unverified source")
	}
	if !installed {
		changed := !homebrewSHA256.MatchString(doc.SHA256) || !doc.AutoUpdates
		for key := range doc.DependsOn {
			changed = changed || key != "macos"
		}
		if changed {
			return errors.New("native app recipe changed its checksum, updater or dependency contract")
		}
	}
	return validateCaskArtifacts(doc.Artifacts, s)
}

func caskCanonical(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	var plain any
	if json.Unmarshal(data, &plain) != nil {
		return ""
	}
	data, err = json.Marshal(plain)
	if err != nil {
		return ""
	}
	return string(data)
}

func (s CaskSpec) artifactForms(a CaskArtifact) (values, targets []string) {
	base := filepath.Base(s.Application)
	for _, source := range []string{"$APPDIR/" + base + "/" + a.Source, s.source(a)} {
		if a.Kind == "command_wrapper" {
			values = append(values, caskCanonical([]any{s.Token, map[string]string{"executable": source}}))
		} else {
			values = append(values, caskCanonical([]string{source}))
		}
	}
	return values, []string{"$HOMEBREW_PREFIX/" + a.Target, s.target(a)}
}

func validateCaskArtifacts(artifacts []map[string]json.RawMessage, s CaskSpec) error {
	app, uninstall := false, false
	used := make([]bool, len(s.Artifacts))
	for _, artifact := range artifacts {
		kind := ""
		for key := range artifact {
			if key != "target" {
				if kind != "" {
					return errors.New("ambiguous native app artifact")
				}
				kind = key
			}
		}
		if kind == "zap" {
			var zap []map[string]json.RawMessage
			if json.Unmarshal(artifact[kind], &zap) != nil {
				return errors.New("invalid unused zap metadata")
			}
			for _, item := range zap {
				for key := range item {
					if key != "trash" && key != "rmdir" {
						return errors.New("unsupported zap metadata")
					}
				}
			}
			continue
		}
		var value any
		if json.Unmarshal(artifact[kind], &value) != nil {
			return errors.New("invalid native app artifact")
		}
		actual := caskCanonical(value)
		target, targeted := "", false
		if raw, ok := artifact["target"]; ok {
			if json.Unmarshal(raw, &target) != nil {
				return errors.New("invalid native app target")
			}
			targeted = true
		}
		switch kind {
		case "app":
			if app {
				return errors.New("duplicate native app artifact")
			}
			app = true
			if actual != caskCanonical([]string{filepath.Base(s.Application)}) {
				return errors.New("native app artifact differs from its reviewed contract")
			}
			if targeted && target != s.Application {
				return errors.New("native app artifact was redirected")
			}
		case "uninstall":
			if uninstall || targeted || actual != caskCanonical([]map[string]string{s.Uninstall}) {
				return errors.New("native app artifact differs from its reviewed contract")
			}
			uninstall = true
		default:
			matched := false
			for i, reviewed := range s.Artifacts {
				if used[i] || reviewed.Kind != kind {
					continue
				}
				values, targets := s.artifactForms(reviewed)
				if actual != values[0] && actual != values[1] {
					continue
				}
				if targeted && target != targets[0] && target != targets[1] {
					return errors.New("native app artifact was redirected")
				}
				used[i], matched = true, true
				break
			}
			if !matched {
				return fmt.Errorf("native app recipe contains unsupported artifact %s", kind)
			}
		}
	}
	if !app {
		return errors.New("native app recipe omitted its application")
	}
	return nil
}

// InstalledRecipe checks the recipe that Homebrew saved for the installed
// cask and uses for a removal or a reinstall: the application folder, the
// homebrew/cask tap, no uninstall script and only the reviewed artifacts.
func (s CaskSpec) InstalledRecipe() error {
	root := filepath.Join(s.Caskroom(), ".metadata")
	configPath := filepath.Join(root, "config.json")
	bound, err := platform.BindDestination(configPath)
	if err != nil || bound != configPath {
		return errors.Join(errors.New("native app metadata was redirected"), err)
	}
	data, err := platform.ReadDocument(configPath)
	if err != nil {
		return err
	}
	var config map[string]map[string]json.RawMessage
	if json.Unmarshal(data, &config) != nil || config["default"] == nil {
		return errors.New("native app configuration is incomplete")
	}
	appdir := ""
	for _, layer := range []string{"default", "env", "explicit"} {
		for key, value := range config[layer] {
			if key == "appdir" {
				if json.Unmarshal(value, &appdir) != nil {
					return errors.New("invalid native app directory")
				}
			}
			if (layer == "env" || layer == "explicit") && key != "appdir" && key != "languages" {
				return errors.New("native app configuration has an unsupported override")
			}
		}
	}
	if appdir != filepath.Dir(s.Application) {
		return errors.New("native app install directory changed")
	}
	paths, err := filepath.Glob(filepath.Join(root, "*", "*", "Casks", "*"))
	if err != nil {
		return err
	}
	if len(paths) != 1 || filepath.Base(paths[0]) != s.Token+".json" {
		return errors.New("native app uninstall recipe is ambiguous or executable")
	}
	bound, err = platform.BindDestination(paths[0])
	if err != nil || bound != paths[0] {
		return errors.Join(errors.New("native app recipe was redirected"), err)
	}
	data, err = platform.ReadDocument(paths[0])
	if err != nil {
		return err
	}
	var compact map[string]json.RawMessage
	if json.Unmarshal(data, &compact) != nil {
		return errors.New("invalid native app installed metadata")
	}
	if len(compact) != 0 {
		return ValidateCaskMetadata(data, s, true)
	}
	data, err = platform.ReadDocument(filepath.Join(root, "INSTALL_RECEIPT.json"))
	if err != nil {
		return err
	}
	var receipt struct {
		FlightBlocks *bool `json:"uninstall_flight_blocks"`
		Source       struct {
			Tap string `json:"tap"`
		} `json:"source"`
		Artifacts []map[string]json.RawMessage `json:"uninstall_artifacts"`
	}
	if len(data) > 1<<20 || json.Unmarshal(data, &receipt) != nil || receipt.FlightBlocks == nil || *receipt.FlightBlocks || receipt.Source.Tap != "homebrew/cask" {
		return errors.New("native app receipt has an unverified source or executable uninstall hooks")
	}
	return validateCaskArtifacts(receipt.Artifacts, s)
}
