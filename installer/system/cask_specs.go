package system

import (
	"errors"
	"path/filepath"
	"strings"
)

// CaskArtifact is one reviewed artifact of a cask besides its application and
// its uninstall entry. Kind is the Homebrew artifact name. Source is a path
// in the application bundle. Target is a path below the Homebrew prefix.
type CaskArtifact struct {
	Kind, Source, Target string
}

// Active reports that Homebrew creates the artifact also with
// --no-binaries: a manual page or a shell completion.
func (a CaskArtifact) Active() bool {
	return a.Kind != "binary" && a.Kind != "command_wrapper"
}

// CaskSpec is the reviewed recipe of one application cask. Application is
// the bundle that setup installs. UserApplication is a bundle of the same
// application that the operator installed in the home folder. Bundle is the
// identifier in the code signature and Team is the Apple team of the
// developer certificate: they give the signing requirement. Uninstall is the
// one reviewed uninstall entry. Command and CommandPath name the command that
// setup links to a path in the bundle. Version and bundle bytes are not part
// of the recipe, because the application updates itself.
type CaskSpec struct {
	Resource, Token, Name        string
	Application, UserApplication string
	Prefix                       string
	Bundle, Team                 string
	Uninstall                    map[string]string
	Artifacts                    []CaskArtifact
	Command, CommandPath         string
}

// Requirement returns the code signing requirement of the application, or ""
// when the recipe has no reviewed identity.
func (s CaskSpec) Requirement() string {
	if s.Bundle == "" || s.Team == "" {
		return ""
	}
	return `anchor apple generic and identifier "` + s.Bundle + `" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = "` + s.Team + `"`
}

// Caskroom returns the folder of the installed cask.
func (s CaskSpec) Caskroom() string {
	return filepath.Join(s.Prefix, "Caskroom", s.Token)
}

// CommandTarget returns the path that the managed command link points at in
// the bundle at application, or "" when the cask has no managed command.
func (s CaskSpec) CommandTarget(application string) string {
	if s.Command == "" {
		return ""
	}
	return filepath.Join(application, filepath.FromSlash(s.CommandPath))
}

func (s CaskSpec) target(a CaskArtifact) string {
	return filepath.Join(s.Prefix, filepath.FromSlash(a.Target))
}

func (s CaskSpec) source(a CaskArtifact) string {
	return filepath.Join(s.Application, filepath.FromSlash(a.Source))
}

// Validate refuses a recipe without a reviewed signing identity or with a
// path that is not clean and absolute. Setup neither installs nor verifies
// such an application.
func (s CaskSpec) Validate() error {
	if !brewPackageName.MatchString(s.Token) || s.Name == "" {
		return errors.New("invalid native application recipe")
	}
	if s.Requirement() == "" || strings.ContainsAny(s.Bundle+s.Team, "\"\\\x00\r\n") {
		return errors.New("the signing requirement of " + s.Name + " is not reviewed in this release; setup does not install or verify the application")
	}
	for _, path := range []string{s.Application, s.UserApplication, s.Prefix} {
		if !macosCleanPath(path) {
			return errors.New("invalid native application path")
		}
	}
	for _, a := range s.Artifacts {
		if a.Kind == "" || a.Source == "" || a.Target == "" || !filepath.IsLocal(filepath.FromSlash(a.Source)) || !filepath.IsLocal(filepath.FromSlash(a.Target)) {
			return errors.New("invalid native application artifact")
		}
	}
	if s.Command != "" && !filepath.IsLocal(filepath.FromSlash(s.CommandPath)) {
		return errors.New("invalid native application command")
	}
	return nil
}

// MacCasks returns the reviewed cask recipes by resource ID for a home
// folder and a Homebrew prefix. The applications go to /Applications.
func MacCasks(home, prefix string) map[string]CaskSpec {
	return macCasks(home, prefix, "/Applications")
}

func macCasks(home, prefix, appdir string) map[string]CaskSpec {
	specs := []CaskSpec{
		{Resource: "app.zen", Token: "zen", Name: "Zen", Application: "Zen.app", Bundle: "app.zen-browser.zen", Team: "9V5K9TP787",
			Artifacts: []CaskArtifact{{"command_wrapper", "Contents/MacOS/zen", "bin/zen"}}},
		{Resource: "app.t3-code", Token: "t3-code", Name: "T3 Code", Application: "T3 Code (Beta).app", Bundle: "com.t3tools.t3code", Team: "ARK85ZXQ4Z",
			Artifacts: []CaskArtifact{{"command_wrapper", "Contents/MacOS/t3-code", "bin/t3-code"}}},
		{Resource: "tool.vscode", Token: "visual-studio-code", Name: "VS Code", Application: "Visual Studio Code.app", Bundle: "com.microsoft.VSCode", Team: "UBF8T346G9",
			Uninstall: map[string]string{"launchctl": "com.microsoft.VSCode.ShipIt", "quit": "com.microsoft.VSCode"},
			Artifacts: []CaskArtifact{
				{"binary", "Contents/Resources/app/bin/code", "bin/code"},
				{"binary", "Contents/Resources/app/bin/code-tunnel", "bin/code-tunnel"},
			},
			Command: "code", CommandPath: "Contents/Resources/app/bin/code"},
		{Resource: "tool.ghostty", Token: "ghostty", Name: "Ghostty", Application: "Ghostty.app", Bundle: "com.mitchellh.ghostty", Team: "24VZTF6M5V",
			Artifacts: []CaskArtifact{
				{"manpage", "Contents/Resources/man/man5/ghostty.5", "share/man/man5/ghostty.5"},
				{"manpage", "Contents/Resources/man/man1/ghostty.1", "share/man/man1/ghostty.1"},
				{"zsh_completion", "Contents/Resources/zsh/site-functions/_ghostty", "share/zsh/site-functions/_ghostty"},
				{"bash_completion", "Contents/Resources/bash-completion/completions/ghostty.bash", "etc/bash_completion.d/ghostty"},
				{"fish_completion", "Contents/Resources/fish/vendor_completions.d/ghostty.fish", "share/fish/vendor_completions.d/ghostty.fish"},
			},
			Command: "ghostty", CommandPath: "Contents/MacOS/ghostty"},
		{Resource: "tool.openwhispr", Token: "openwhispr", Name: "OpenWhispr", Application: "OpenWhispr.app", Bundle: "com.gizmolabs.openwhispr", Team: "T832773L2J"},
	}
	result := map[string]CaskSpec{}
	for _, s := range specs {
		s.UserApplication = filepath.Join(home, "Applications", s.Application)
		s.Application = filepath.Join(appdir, s.Application)
		s.Prefix = prefix
		if s.Uninstall == nil {
			s.Uninstall = map[string]string{"quit": s.Bundle}
		}
		result[s.Resource] = s
	}
	return result
}
