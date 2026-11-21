// Package platform holds host discovery, known folders, the installer lock,
// atomic document IO, file system primitives and the foreground command runner.
package platform

import "fmt"

// Context describes the target system, not the machine that built a plan.
type Context struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// Validate accepts only the supported targets.
func (c Context) Validate() error {
	if c.OS != "darwin" && c.OS != "linux" && c.OS != "windows" {
		return fmt.Errorf("unsupported OS %q", c.OS)
	}
	if c.OS == "darwin" && c.Arch != "arm64" {
		return fmt.Errorf("macOS requires Apple Silicon and the native arm64 installer; %s is unsupported", c.Arch)
	}
	if c.Arch != "amd64" && c.Arch != "arm64" {
		return fmt.Errorf("unsupported architecture %q", c.Arch)
	}
	if c.OS == "windows" && c.Arch != "amd64" {
		return fmt.Errorf("unsupported or unaudited target %s/%s", c.OS, c.Arch)
	}
	return nil
}
