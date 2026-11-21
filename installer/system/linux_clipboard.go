package system

import "os"

// LinuxClipboard describes the clipboard transports that the session of the
// process advertises. It holds no display address and no other inherited
// value. Both APT clipboard helpers stay requirements, so an installation
// from a headless session also works in a later graphical session.
type LinuxClipboard struct {
	Wayland bool
	X11     bool
	Tmux    bool
}

// DiscoverLinuxClipboard reads the session at the process boundary.
func DiscoverLinuxClipboard() LinuxClipboard {
	return LinuxClipboard{Wayland: os.Getenv("WAYLAND_DISPLAY") != "", X11: os.Getenv("DISPLAY") != "", Tmux: os.Getenv("TMUX") != ""}
}

// Disclosures names the clipboard transport that the installer checks do not
// exercise. A session without a display does not make the helpers unhealthy.
func (c LinuxClipboard) Disclosures() []string {
	switch {
	case c.Wayland:
		return []string{"Wayland clipboard round-trip in the runtime session (not exercised by installer checks)"}
	case c.X11:
		return []string{"X11 clipboard round-trip in the runtime session (not exercised by installer checks)"}
	case c.Tmux:
		return []string{"headless tmux/terminal clipboard transport (no graphical display advertised; verify OSC 52 in the actual terminal)"}
	default:
		return []string{"headless terminal clipboard transport (no graphical display advertised; verify OSC 52 or a runtime bridge in the actual terminal)"}
	}
}
