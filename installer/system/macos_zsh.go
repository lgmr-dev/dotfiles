package system

import (
	"context"
	"errors"

	"github.com/lgmr-dev/dotfiles/installer/catalog"
	"github.com/lgmr-dev/dotfiles/installer/engine"
)

// MacZsh is the provider of tool.zsh on macOS. The system supplies /bin/zsh.
// Setup uses it and never installs, changes or removes it.
type MacZsh struct{ path string }

// NewMacZsh returns the provider. It is pure.
func NewMacZsh() *MacZsh { return &MacZsh{path: "/bin/zsh"} }

// Inspect reports if the system shell is a regular executable file.
func (d *MacZsh) Inspect(ctx context.Context, _ catalog.Resource, _ engine.Want, _ engine.Records) (engine.Finding, error) {
	if err := ctx.Err(); err != nil {
		return engine.Finding{}, err
	}
	hash, healthy, err := macosSystemFile(d.path, 32<<20)
	if err != nil {
		return engine.Finding{}, err
	}
	if !healthy {
		return macosCheck{Status: engine.Blocked, Reason: "macOS's /bin/zsh is missing or damaged; repair the operating-system component"}.finding()
	}
	return macosCheck{Status: engine.Satisfied, Fingerprint: hash}.finding()
}

// Apply always fails: the system shell is not a package of setup.
func (*MacZsh) Apply(context.Context, catalog.Resource, engine.Finding, engine.Records) error {
	return errors.New("the macOS system shell is not an installer-owned package")
}

// Remove always fails: setup never removes the system shell.
func (*MacZsh) Remove(context.Context, catalog.Resource, engine.Finding, engine.Records) error {
	return errors.New("the macOS system shell cannot be removed by dotfiles")
}
