package builds

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

// RunWindowsRustLauncher runs the original Rust command when the process is
// one of the three launcher copies in a Rust build: rustc.exe, rustdoc.exe and
// clippy-driver.exe. It adds the extended-length sysroot of the build, so the
// compiler works below a long path. An ordinary installer call returns false.
// The main function calls it first.
func RunWindowsRustLauncher() (handled bool, exitCode int) {
	executable, err := os.Executable()
	if err != nil || rustOriginalCommand(filepath.Base(executable)) == "" {
		return false, 0
	}
	if !strings.EqualFold(filepath.Base(filepath.Dir(executable)), "bin") {
		fmt.Fprintln(os.Stderr, "Dotfiles Rust: launcher is outside the managed toolchain bin directory")
		return true, 1
	}
	sysroot := platform.WindowsExtendedPath(filepath.Dir(filepath.Dir(executable)))
	return true, runRustLauncher(executable, sysroot, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}
