package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
)

// MaxArchiveBytes is the size limit of one pinned download.
const MaxArchiveBytes int64 = 1 << 30

// ArchivePin describes upstream bytes and bounded preparation metadata. It
// never supplies a command or a remote script. Paths are portable and
// relative to the extracted payload after StripComponents.
type ArchivePin struct {
	Version             string                  `json:"version"`
	URL                 string                  `json:"url"`
	SHA256              string                  `json:"sha256"`
	Format              string                  `json:"format"`
	AppBundle           string                  `json:"app_bundle,omitempty"`
	DMGRevision         int                     `json:"dmg_revision,omitempty"`
	StripComponents     int                     `json:"strip_components,omitempty"`
	File                string                  `json:"file,omitempty"`
	Commands            map[string]string       `json:"commands"`
	BinDirs             []string                `json:"bin_dirs"`
	RequiredFiles       []string                `json:"required_files,omitempty"`
	ExcludedFiles       []string                `json:"excluded_files,omitempty"`
	Replacements        []ArchiveReplacement    `json:"replacements,omitempty"`
	Libc                string                  `json:"libc,omitempty"`
	PythonStdlib        string                  `json:"python_stdlib,omitempty"`
	RustTarget          string                  `json:"rust_target,omitempty"`
	RustComponents      []ArchivePin            `json:"rust_components,omitempty"`
	WindowsRustLauncher *WindowsRustLauncherPin `json:"windows_rust_launcher,omitempty"`
	Latex2text          *Latex2textPin          `json:"latex2text,omitempty"`
	Yamllint            *YamllintPin            `json:"yamllint,omitempty"`
	WindowsVenvRevision int                     `json:"windows_venv_revision,omitempty"`
	PortableGit         bool                    `json:"portable_git,omitempty"`
	PortableGitRevision int                     `json:"portable_git_revision,omitempty"`
	ZenProfileRevision  int                     `json:"zen_profile_revision,omitempty"`
	ZenProfile          bool                    `json:"zen_profile,omitempty"`
	GhosttyLibraries    bool                    `json:"ghostty_libraries,omitempty"`
	T3CodeLauncher      bool                    `json:"t3_code_launcher,omitempty"`
	AzureCLI            *AzureCLIPin            `json:"azure_cli,omitempty"`
}

// ArchiveReplacement is one reviewed correction of a verified payload file.
// It is data, never a command. Count is the exact number of matches, one by
// default.
type ArchiveReplacement struct {
	File   string `json:"file"`
	Before string `json:"before"`
	After  string `json:"after"`
	Count  int    `json:"count,omitempty"`
}

// WindowsRustLauncherPin declares the launcher recipe revision. The process
// boundary supplies the hash of the installer executable.
type WindowsRustLauncherPin struct {
	Revision int    `json:"revision"`
	SHA256   string `json:"sha256,omitempty"`
}

// Latex2textPin holds the build backend of the latex2text recipe.
// Pins.Archive fills PythonPinID from the Python archive of the same system.
type Latex2textPin struct {
	Backend         ArchivePin `json:"backend"`
	PythonPinID     string     `json:"python_pin_id"`
	ConsoleRevision int        `json:"console_revision,omitempty"`
}

// YamllintPin is the fixed wheel closure of yamllint: pathspec and PyYAML.
// Pins.Archive fills PythonPinID from the Python archive of the same system.
type YamllintPin struct {
	Pathspec    ArchivePin `json:"pathspec"`
	PyYAML      ArchivePin `json:"pyyaml"`
	PythonPinID string     `json:"python_pin_id"`
}

// AzureCLIPin adds the Python interpreter that the compiled extensions of
// the POSIX Azure CLI archives require.
type AzureCLIPin struct {
	Python ArchivePin `json:"python"`
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)
var pythonToolVersion = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)+$`)
var portableGitVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.windows\.[0-9]+$`)
var yamllintPyYAMLWheel = regexp.MustCompile(`^pyyaml-[0-9]+(?:\.[0-9]+)+-(cp[0-9]+)-(cp[0-9]+)-(macosx_11_0_arm64|manylinux2014_x86_64\.manylinux_2_17_x86_64\.manylinux_2_28_x86_64|manylinux2014_aarch64\.manylinux_2_17_aarch64\.manylinux_2_28_aarch64|win_amd64)\.whl$`)

var portableGitRequiredFiles = []string{
	"cmd/git.exe", "cmd/git-gui.exe", "cmd/gitk.exe", "bin/bash.exe", "bin/sh.exe",
	"git-bash.exe", "git-cmd.exe", "usr/bin/bash.exe", "usr/bin/ssh.exe", "usr/bin/ssh-keygen.exe",
	"usr/bin/msys-2.0.dll", "usr/bin/msys-crypto-3.dll", "usr/bin/msys-z.dll",
	"usr/bin/msys-iconv-2.dll", "usr/bin/msys-intl-8.dll", "usr/bin/msys-readline8.dll", "usr/bin/msys-ncursesw6.dll",
	"ucrt64/bin/git.exe", "ucrt64/bin/git-lfs.exe", "ucrt64/bin/git-credential-manager.exe",
	"ucrt64/bin/libcurl-4.dll", "ucrt64/bin/libpcre2-8-0.dll", "ucrt64/bin/libiconv-2.dll", "ucrt64/bin/libintl-8.dll",
	"ucrt64/bin/libcrypto-3-x64.dll", "ucrt64/bin/libssl-3-x64.dll", "ucrt64/bin/zlib1.dll",
	"ucrt64/bin/tcl86.dll", "ucrt64/bin/tk86.dll", "ucrt64/libexec/git-core/git-gui",
	"etc/gitconfig", "etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", "README.portable",
}

const azureCLIPackages = "libexec/lib/python3.14/site-packages"
const azureCLIPython = "bin/python3.14"
const azureCLICommand = "commands/az"
const azureCLIMain = azureCLIPackages + "/azure/cli/__main__.py"
const t3CodeBinary = "opt/T3 Code (Beta)/t3code"

// ArchivePinID returns the identity of a pin: the SHA-256 of its JSON form.
func ArchivePinID(pin ArchivePin) string {
	data, err := json.Marshal(pin)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// PortablePath reports if name is a relative path with one meaning on each
// system. It applies the Windows file name limits everywhere.
func PortablePath(name string) bool {
	if !canonicalRelative(name) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.TrimRight(part, ". ") != part || strings.ContainsAny(part, "<>\"|?*\x00") {
			return false
		}
		base := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if slices.Contains([]string{"CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$"}, base) || len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
			return false
		}
	}
	return true
}

// Validate accepts only a complete pin with a supported layout and recipe.
func (p ArchivePin) Validate() error {
	if p.Format == "dmg" && p.DMGRevision != 1 || p.Format != "dmg" && p.DMGRevision != 0 {
		return errors.New("disk-image transport revision requires its supported DMG layout")
	}
	if p.PythonStdlib != "" && (!PortablePath(p.PythonStdlib) || p.Commands["python"] == "" && p.Commands["python3"] == "") {
		return errors.New("Python bytecode preparation requires a declared interpreter and relative standard library")
	}
	if p.Libc != "" && p.Libc != "glibc" && p.Libc != "musl" {
		return errors.New("unsupported archive libc requirement")
	}
	u, err := url.Parse(p.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || !sha256Hex.MatchString(p.SHA256) || p.Version == "" {
		return errors.New("archive requires a version, HTTPS URL and SHA-256 pin")
	}
	if !slices.Contains([]string{"tar.gz", "tar.xz", "dmg", "zip", "gzip", "file", "deb"}, p.Format) || p.StripComponents < 0 || p.StripComponents > 4 || (len(p.Commands) == 0 && len(p.RequiredFiles) == 0) || (len(p.Commands) > 0 && len(p.BinDirs) == 0) {
		return errors.New("invalid archive format, layout or command list")
	}
	if p.Format == "dmg" {
		if !PortablePath(p.AppBundle) || strings.Contains(p.AppBundle, "/") || !strings.HasSuffix(p.AppBundle, ".app") || p.StripComponents != 0 {
			return errors.New("disk image requires one top-level application bundle")
		}
	} else if p.AppBundle != "" {
		return errors.New("application bundle selection requires a disk image")
	}
	if (p.Format == "gzip" || p.Format == "file") && (!PortablePath(p.File) || p.StripComponents != 0) {
		return errors.New("single-file archive requires a safe output filename")
	}
	for name, file := range p.Commands {
		if !resourceID.MatchString(name) || !PortablePath(file) {
			return errors.New("invalid archive command mapping")
		}
	}
	for _, file := range p.RequiredFiles {
		if !PortablePath(file) {
			return errors.New("invalid required archive file")
		}
	}
	for i, file := range p.ExcludedFiles {
		if !PortablePath(file) || slices.Contains(p.ExcludedFiles[:i], file) || slices.Contains(p.RequiredFiles, file) {
			return errors.New("invalid, repeated or required archive exclusion")
		}
		for _, command := range p.Commands {
			if file == command {
				return errors.New("archive exclusion removes a declared command")
			}
		}
	}
	for _, replacement := range p.Replacements {
		if !PortablePath(replacement.File) || slices.Contains(p.ExcludedFiles, replacement.File) || replacement.Before == "" || replacement.Before == replacement.After || replacement.Count < 0 || len(replacement.Before) > 1<<20 || len(replacement.After) > 1<<20 {
			return errors.New("invalid reviewed archive replacement")
		}
	}
	for _, dir := range p.BinDirs {
		if dir != "." && !PortablePath(dir) {
			return errors.New("invalid archive binary directory")
		}
	}
	return p.validatePreparation()
}

func (p ArchivePin) validatePreparation() error {
	latexConsole := p.Latex2text != nil && slices.Contains([]string{"Scripts/latex2text.exe", "commands/latex2text.exe"}, p.Commands["latex2text"])
	yamllintConsole := p.Yamllint != nil && slices.Contains([]string{"Scripts/yamllint.exe", "commands/yamllint.exe"}, p.Commands["yamllint"])
	if p.WindowsVenvRevision < 0 || p.WindowsVenvRevision > 1 || p.WindowsVenvRevision != 0 && !latexConsole && !yamllintConsole {
		return errors.New("Windows Python recipe revision requires its fixed private environment")
	}
	if (p.Latex2text != nil && strings.HasSuffix(p.Commands["latex2text"], ".exe") || p.Yamllint != nil && strings.HasSuffix(p.Commands["yamllint"], ".exe")) && p.WindowsVenvRevision < 1 {
		return errors.New("Windows Python recipe requires its current private environment revision")
	}
	if launcher := p.WindowsRustLauncher; launcher != nil && (p.RustTarget != "x86_64-pc-windows-msvc" || len(p.RustComponents) == 0 || launcher.Revision != 1 || launcher.SHA256 != "" && !sha256Hex.MatchString(launcher.SHA256)) {
		return errors.New("Windows Rust launcher requires its fixed recipe and executable SHA-256")
	}
	if !p.ZenProfile && p.ZenProfileRevision != 0 {
		return errors.New("Zen recipe revision requires its fixed preparation")
	}
	if !p.PortableGit && p.PortableGitRevision != 0 {
		return errors.New("PortableGit recipe revision requires its fixed preparation")
	}
	preparations := 0
	for _, enabled := range []bool{len(p.RustComponents) > 0, p.Latex2text != nil, p.Yamllint != nil, p.PythonStdlib != "", p.PortableGit, p.GhosttyLibraries, p.ZenProfile, p.T3CodeLauncher, p.AzureCLI != nil} {
		if enabled {
			preparations++
		}
	}
	if preparations > 1 {
		return errors.New("archive preparations cannot be combined")
	}
	if len(p.RustComponents) > 0 {
		if err := p.validateRust(); err != nil {
			return err
		}
	}
	switch {
	case len(p.RustComponents) == 0 && p.RustTarget != "":
		return errors.New("Rust target requires its complete component set")
	case p.Latex2text != nil:
		return p.validateLatex2text()
	case p.Yamllint != nil:
		return p.validateYamllint()
	case p.PortableGit:
		return p.validatePortableGit()
	case p.ZenProfile:
		return p.validateZen()
	case p.GhosttyLibraries:
		return p.validateGhosttyLibraries()
	case p.T3CodeLauncher:
		return p.validateT3CodeLauncher()
	case p.AzureCLI != nil:
		return p.validateAzureCLI()
	}
	return nil
}

// RustComponentNames returns the five components that complete the Rust
// compiler archive of a target, in pin order.
func RustComponentNames(target string) []string {
	return []string{"cargo", "rust-std-" + target, "rustfmt-preview", "clippy-preview", "rust-src"}
}

func (p ArchivePin) nested() bool {
	return len(p.RustComponents) != 0 || p.RustTarget != "" || p.Latex2text != nil || p.Yamllint != nil || p.PythonStdlib != "" || p.PortableGit
}

func (p ArchivePin) validateRust() error {
	if !slices.Contains([]string{"aarch64-apple-darwin", "x86_64-unknown-linux-gnu", "aarch64-unknown-linux-gnu", "x86_64-pc-windows-msvc"}, p.RustTarget) {
		return errors.New("unsupported Rust toolchain target")
	}
	suffix := ""
	if p.RustTarget == "x86_64-pc-windows-msvc" {
		suffix = ".exe"
	}
	commands := map[string]string{}
	for _, name := range []string{"rustc", "cargo", "rustfmt", "cargo-fmt", "cargo-clippy", "clippy-driver"} {
		commands[name] = "bin/" + name + suffix
	}
	if len(p.RustComponents) != 5 || p.Format != "tar.gz" || p.StripComponents != 1 || !maps.Equal(p.Commands, commands) || !slices.Equal(p.BinDirs, []string{"bin"}) || !slices.Contains(p.RequiredFiles, "lib/rustlib/src/rust/library/core/src/lib.rs") {
		return errors.New("Rust preparation requires the fixed component layout")
	}
	expected := RustComponentNames(p.RustTarget)
	for index, component := range p.RustComponents {
		if component.nested() || component.GhosttyLibraries || component.ZenProfile || component.T3CodeLauncher || component.AzureCLI != nil {
			return errors.New("nested archive preparations are forbidden")
		}
		if component.Version != p.Version || component.Format != "tar.gz" || component.StripComponents != 1 || len(component.Commands) != 0 || !slices.Equal(component.RequiredFiles, []string{expected[index] + "/manifest.in"}) || len(component.ExcludedFiles) != 0 || len(component.Replacements) != 0 {
			return fmt.Errorf("invalid fixed Rust component %s", expected[index])
		}
		if err := component.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (p ArchivePin) validateConsole(name string) error {
	target := p.Commands[name]
	if !slices.Contains([]string{"bin/" + name, "commands/" + name, "Scripts/" + name + ".exe", "commands/" + name + ".exe"}, target) {
		return fmt.Errorf("%s requires its fixed private console command", name)
	}
	if len(p.Commands) != 1 || !slices.Equal(p.BinDirs, []string{path.Dir(target)}) {
		return fmt.Errorf("%s has an unexpected command layout", name)
	}
	return nil
}

func (p ArchivePin) validateLatex2text() error {
	recipe := p.Latex2text
	if p.Format != "file" || p.File != "pylatexenc-"+p.Version+".tar.gz" || !pythonToolVersion.MatchString(p.Version) || !sha256Hex.MatchString(recipe.PythonPinID) || recipe.ConsoleRevision < 0 || recipe.ConsoleRevision > 1 {
		return errors.New("latex2text requires a pinned source distribution and Python archive identity")
	}
	if err := p.validateConsole("latex2text"); err != nil {
		return err
	}
	backend := recipe.Backend
	if backend.nested() || backend.Format != "file" || backend.File != "setuptools-"+backend.Version+"-py3-none-any.whl" || !pythonToolVersion.MatchString(backend.Version) || len(backend.Commands) != 0 {
		return errors.New("latex2text requires its pinned setuptools wheel")
	}
	return backend.Validate()
}

func (p ArchivePin) validateYamllint() error {
	recipe := p.Yamllint
	if p.Format != "file" || p.File != "yamllint-"+p.Version+"-py3-none-any.whl" || !pythonToolVersion.MatchString(p.Version) || !sha256Hex.MatchString(recipe.PythonPinID) {
		return errors.New("yamllint requires its pinned wheel and Python archive identity")
	}
	if err := p.validateConsole("yamllint"); err != nil {
		return err
	}
	if recipe.Pathspec.File != "pathspec-"+recipe.Pathspec.Version+"-py3-none-any.whl" {
		return errors.New("yamllint requires the pinned pathspec wheel")
	}
	match := yamllintPyYAMLWheel.FindStringSubmatch(recipe.PyYAML.File)
	if len(match) != 4 || match[1] != match[2] || !strings.HasPrefix(recipe.PyYAML.File, "pyyaml-"+recipe.PyYAML.Version+"-") {
		return errors.New("yamllint requires a supported PyYAML wheel")
	}
	for _, wheel := range []ArchivePin{recipe.Pathspec, recipe.PyYAML} {
		if !pythonToolVersion.MatchString(wheel.Version) || wheel.Format != "file" || len(wheel.Commands) != 0 || len(wheel.BinDirs) != 0 || !slices.Equal(wheel.RequiredFiles, []string{wheel.File}) ||
			wheel.nested() || len(wheel.ExcludedFiles) != 0 || len(wheel.Replacements) != 0 {
			return errors.New("yamllint dependencies must be checksum-pinned wheels without nested preparation")
		}
		if err := wheel.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (p ArchivePin) validatePortableGit() error {
	version := strings.ReplaceAll(p.Version, ".windows.", ".")
	if strings.HasSuffix(p.Version, ".windows.1") {
		version = strings.TrimSuffix(p.Version, ".windows.1")
	}
	u, err := url.Parse(p.URL)
	if p.PortableGitRevision < 0 || p.PortableGitRevision > 1 || !portableGitVersion.MatchString(p.Version) || p.Format != "file" || p.File != "portable-git.exe" || p.StripComponents != 0 || len(p.ExcludedFiles) != 0 || len(p.Replacements) != 0 ||
		err != nil || u.Scheme != "https" || u.Host != "github.com" || u.RawQuery != "" || u.User != nil || u.Fragment != "" || u.Path != "/git-for-windows/git/releases/download/v"+p.Version+"/PortableGit-"+version+"-64-bit.7z.exe" ||
		p.Commands["git"] != "cmd/git.exe" || p.Commands["bash"] != "bin/bash.exe" || p.Commands["sh"] != "bin/sh.exe" {
		return errors.New("PortableGit requires the pinned complete official amd64 self-extractor and fixed command layout")
	}
	for _, name := range portableGitRequiredFiles {
		if !slices.Contains(p.RequiredFiles, name) {
			return fmt.Errorf("PortableGit requires runtime file %s", name)
		}
	}
	return nil
}

func (p ArchivePin) validateZen() error {
	if p.ZenProfileRevision != 1 {
		return errors.New("unsupported Zen preparation revision")
	}
	if p.Format != "tar.xz" || p.StripComponents != 1 || !maps.Equal(p.Commands, map[string]string{"zen": "zen"}) || !slices.Equal(p.BinDirs, []string{"."}) || !slices.Contains(p.RequiredFiles, "zen-bin") {
		return errors.New("Zen preparation requires its official Linux archive layout")
	}
	return nil
}

func (p ArchivePin) validateGhosttyLibraries() error {
	if p.Format != "deb" || p.StripComponents != 0 || len(p.Commands) != 1 || p.Commands["ghostty"] != "usr/bin/ghostty" || !slices.Equal(p.BinDirs, []string{"usr/bin"}) || len(p.ExcludedFiles) != 0 || len(p.Replacements) != 0 {
		return errors.New("Ghostty library preparation requires its fixed Debian package layout")
	}
	for _, name := range []string{"usr/bin/ghostty-bin", "usr/lib/libgtk4-layer-shell.so", "usr/share/terminfo/x/xterm-ghostty", "usr/share/ghostty/themes/Rose Pine"} {
		if !slices.Contains(p.RequiredFiles, name) {
			return errors.New("Ghostty library preparation lacks its required private runtime")
		}
	}
	return nil
}

func (p ArchivePin) validateT3CodeLauncher() error {
	if p.Format != "deb" || p.StripComponents != 0 || len(p.Commands) != 0 || len(p.ExcludedFiles) != 0 || len(p.Replacements) != 0 || !slices.Contains(p.RequiredFiles, t3CodeBinary) || !slices.Contains(p.RequiredFiles, "t3-code") {
		return errors.New("T3 Code launcher preparation requires its fixed Debian package layout")
	}
	return nil
}

func (p ArchivePin) validateAzureCLI() error {
	if p.Format != "tar.gz" || p.StripComponents != 0 || !maps.Equal(p.Commands, map[string]string{"az": azureCLICommand}) || !slices.Equal(p.BinDirs, []string{"commands"}) || !slices.Equal(p.RequiredFiles, []string{azureCLIMain + "c"}) || len(p.ExcludedFiles) != 0 || len(p.Replacements) != 0 {
		return errors.New("Azure CLI preparation requires its official archive layout")
	}
	python := p.AzureCLI.Python
	plain := ArchivePin{Version: python.Version, URL: python.URL, SHA256: python.SHA256, Format: "tar.gz", StripComponents: 1, Commands: map[string]string{}, BinDirs: []string{}, RequiredFiles: []string{azureCLIPython}}
	if !strings.HasPrefix(python.Version, "3.14.") || ArchivePinID(python) != ArchivePinID(plain) {
		return errors.New("Azure CLI requires its plain pinned Python 3.14 archive")
	}
	return python.Validate()
}

func bindYamllintPython(pin, python ArchivePin, key string) (ArchivePin, error) {
	tags := map[string]string{
		"darwin/arm64":  "macosx_11_0_arm64",
		"linux/amd64":   "manylinux2014_x86_64.manylinux_2_17_x86_64.manylinux_2_28_x86_64",
		"linux/arm64":   "manylinux2014_aarch64.manylinux_2_17_aarch64.manylinux_2_28_aarch64",
		"windows/amd64": "win_amd64",
	}
	parts := strings.Split(python.Version, ".")
	if len(parts) < 2 || !pythonToolVersion.MatchString(python.Version) || tags[key] == "" {
		return pin, errors.New("yamllint requires its selected supported Python archive")
	}
	abi := "cp" + parts[0] + parts[1]
	recipe := *pin.Yamllint
	if recipe.PyYAML.File != "pyyaml-"+recipe.PyYAML.Version+"-"+abi+"-"+abi+"-"+tags[key]+".whl" {
		return pin, fmt.Errorf("yamllint PyYAML wheel does not match Python %s on %s", python.Version, key)
	}
	recipe.PythonPinID = ArchivePinID(python)
	pin.Yamllint = &recipe
	return pin, nil
}
