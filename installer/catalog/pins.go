package catalog

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

// ErrNoArchive reports that a family has no archive pin for a host.
var ErrNoArchive = errors.New("no archive pin for this system")

// Pins holds the reviewed pins of every tool folder, in the v2 file formats.
// Archives maps a family (a resource ID) and a platform key to its pin as the
// file states it. Archive returns the pin that a build uses.
type Pins struct {
	Archives       map[string]map[string]ArchivePin
	LinuxCode      map[string]LinuxCodePin
	LinuxCodeModes map[string]LinuxCodeModes
	LinuxPackages  map[string]map[string]LinuxPackagePin
	BuildTools     *WindowsBuildToolsPin
	VCRedist       *WindowsVendorPin
	WindowsApps    map[string]WindowsAppPin
}

// LinuxCodePin is the official Code DEB of one architecture.
type LinuxCodePin struct {
	Version      string `json:"version"`
	URL          string `json:"url"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
	Architecture string `json:"architecture"`
}

// LinuxCodeModes declares the file modes that one Code version installs.
type LinuxCodeModes struct {
	Sandbox  string `json:"sandbox"`
	RootOnly string `json:"root_only,omitempty"`
}

// LinuxPackagePin is a reviewed vendor DEB of one architecture.
type LinuxPackagePin struct {
	Package      string `json:"package"`
	Version      string `json:"version"`
	Architecture string `json:"architecture"`
	URL          string `json:"url"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
}

// WindowsVendorPin identifies a reviewed Microsoft VC++ x64 runtime bundle.
// BundleID is the exact registered Burn bundle.
type WindowsVendorPin struct {
	Version  string `json:"version"`
	URL      string `json:"url"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	BundleID string `json:"bundle_id"`
}

// WindowsBuildToolsPin identifies the reviewed Build Tools bootstrapper and
// the components that setup requests.
type WindowsBuildToolsPin struct {
	Version, URL, SHA256 string
	Size                 int64
	Components           []string
}

// WindowsAppPin identifies a reviewed per-user NSIS installer and the
// application that it registers.
type WindowsAppPin struct {
	Version     string `json:"version"`
	URL         string `json:"url"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	ProductCode string `json:"product_code"`
	Publisher   string `json:"publisher"`
	Folder      string `json:"folder"`
	Executable  string `json:"executable"`
	Uninstaller string `json:"uninstaller"`
}

const distroReleaseNumber = `[0-9]+(\.[0-9]+)*`

var distributionReleasePin = regexp.MustCompile("^(ubuntu|debian)-" + distroReleaseNumber + "$")
var linuxCodeVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+-[0-9]+$`)
var linuxCodeURLPath = regexp.MustCompile(`^/dbazure/download/stable/[a-f0-9]{40}/code_[0-9]+\.[0-9]+\.[0-9]+-[0-9]+_(amd64|arm64)\.deb$`)
var linuxCodeRootOnly = regexp.MustCompile(`^/usr/share/code(/[A-Za-z0-9_-][A-Za-z0-9_.-]*)+$`)
var linuxPackageVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
var windowsVendorVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$`)
var windowsVendorGUID = regexp.MustCompile(`^\{[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}\}$`)
var windowsAppGUID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var windowsAppVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}$`)
var windowsBuildToolsComponents = []string{"Microsoft.VisualStudio.Workload.VCTools", "Microsoft.VisualStudio.Component.VC.Tools.x86.x64", "Microsoft.VisualStudio.Component.Windows11SDK.26100"}

// Validate accepts only an exact official Code DEB.
func (pin LinuxCodePin) Validate() error {
	u, err := url.Parse(pin.URL)
	if err != nil || u.Scheme != "https" || u.Host != "vscode.download.prss.microsoft.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		!linuxCodeURLPath.MatchString(u.Path) || path.Base(u.Path) != "code_"+pin.Version+"_"+pin.Architecture+".deb" ||
		!linuxCodeVersion.MatchString(pin.Version) || !slices.Contains([]string{"amd64", "arm64"}, pin.Architecture) || !sha256Hex.MatchString(pin.SHA256) || pin.Size <= 0 || pin.Size > MaxArchiveBytes {
		return errors.New("Code requires an exact official Debian package version, architecture, size and SHA-256")
	}
	return nil
}

// Validate accepts only an official release of a supported vendor package.
func (pin LinuxPackagePin) Validate() error {
	if !linuxPackageVersion.MatchString(pin.Version) || !slices.Contains([]string{"amd64", "arm64"}, pin.Architecture) || !sha256Hex.MatchString(pin.SHA256) || pin.Size <= 0 || pin.Size > MaxArchiveBytes {
		return errors.New("native application requires a fixed version, architecture, size and SHA-256")
	}
	u, err := url.Parse(pin.URL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("invalid vendor package URL")
	}
	if pin.Package != "tailscale" {
		return errors.New("unsupported fixed vendor package")
	}
	if pin.URL != "https://pkgs.tailscale.com/stable/tailscale_"+pin.Version+"_"+pin.Architecture+".deb" {
		return errors.New("native package does not match its official release URL")
	}
	return nil
}

// Validate accepts only an exact official VC++ x64 runtime bundle.
func (pin WindowsVendorPin) Validate() error {
	u, err := url.Parse(pin.URL)
	if err != nil || u.Scheme != "https" || u.Host != "download.visualstudio.microsoft.com" || u.User != nil || u.Fragment != "" || u.RawQuery != "" ||
		!strings.HasSuffix(strings.ToLower(u.Path), "/vc_redist.x64.exe") || !sha256Hex.MatchString(pin.SHA256) ||
		!windowsVendorVersion.MatchString(pin.Version) || !strings.HasPrefix(pin.Version, "14.") || pin.Size <= 0 || pin.Size > 128<<20 || !windowsVendorGUID.MatchString(pin.BundleID) {
		return errors.New("VC++ runtime requires an exact official x64 bundle, version, size and SHA-256")
	}
	return nil
}

// Validate accepts only the official bootstrapper with the reviewed C++ and
// Windows SDK components.
func (pin WindowsBuildToolsPin) Validate() error {
	u, err := url.Parse(pin.URL)
	if err != nil || u.Scheme != "https" || u.Host != "download.visualstudio.microsoft.com" || u.User != nil || u.Fragment != "" || u.RawQuery != "" ||
		!strings.HasSuffix(strings.ToLower(u.Path), "/vs_buildtools.exe") || !sha256Hex.MatchString(pin.SHA256) || !windowsVendorVersion.MatchString(pin.Version) || pin.Size <= 0 || pin.Size > 128<<20 || !slices.Equal(pin.Components, windowsBuildToolsComponents) {
		return errors.New("Build Tools requires its reviewed official bootstrapper and exact C++/Windows SDK components")
	}
	return nil
}

func windowsAppName(name string) bool {
	return name != "" && len(name) <= 128 && name == strings.TrimSpace(name) && !strings.ContainsAny(name, "\x00\r\n\"/\\:*?<>|") && name != "." && name != ".."
}

// Validate accepts only an exact official T3 Code x64 installer.
func (pin WindowsAppPin) Validate() error {
	u, err := url.Parse(pin.URL)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.Fragment != "" || u.RawQuery != "" ||
		!strings.HasPrefix(u.Path, "/pingdotgg/t3code/releases/download/v"+pin.Version+"/") || !strings.HasSuffix(u.Path, "-x64.exe") ||
		!sha256Hex.MatchString(pin.SHA256) || !windowsAppVersion.MatchString(pin.Version) || pin.Size <= 0 || pin.Size > MaxArchiveBytes ||
		!windowsAppGUID.MatchString(pin.ProductCode) || !windowsAppName(pin.Publisher) || !windowsAppName(pin.Folder) ||
		!windowsAppName(pin.Executable) || !strings.HasSuffix(pin.Executable, ".exe") || !windowsAppName(pin.Uninstaller) || !strings.HasSuffix(pin.Uninstaller, ".exe") {
		return errors.New("Windows application requires an exact official x64 installer, version, size, SHA-256, publisher and layout")
	}
	return nil
}

func (p *Pins) load(fsys fs.FS, folder string, home map[string]string) error {
	entries, err := fs.ReadDir(fsys, folder)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		file := path.Join(folder, name)
		if name == "tool.json" || name == "shell" && entry.IsDir() {
			continue
		}
		owner := func(id string) error {
			if home[id] != folder {
				return fmt.Errorf("%s: the resource %s is not in this tool folder", file, id)
			}
			return nil
		}
		once := func(present bool) error {
			if present {
				return fmt.Errorf("%s: a second tool folder holds this pin file", file)
			}
			return nil
		}
		data, err := fs.ReadFile(fsys, file)
		if err != nil {
			return err
		}
		switch name {
		case "pins.json":
			var document struct {
				Schema    int                              `json:"schema"`
				Resources map[string]map[string]ArchivePin `json:"resources"`
			}
			if err := platform.Decode(data, &document); err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
			if document.Schema != 1 || len(document.Resources) == 0 {
				return fmt.Errorf("%s: unsupported archive pin schema or no family", file)
			}
			for family, platforms := range document.Resources {
				if err := owner(family); err != nil {
					return err
				}
				p.Archives[family] = platforms
			}
		case "linux-code-pins.json":
			var document struct {
				Schema int                       `json:"schema"`
				Pins   map[string]LinuxCodePin   `json:"pins"`
				Modes  map[string]LinuxCodeModes `json:"modes"`
			}
			if err := errors.Join(once(p.LinuxCode != nil), owner("tool.vscode"), platform.Decode(data, &document)); err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
			if document.Schema != 1 || len(document.Pins) != 2 {
				return fmt.Errorf("%s: invalid Linux Code pins", file)
			}
			p.LinuxCode, p.LinuxCodeModes = document.Pins, document.Modes
		case "linux-package-pins.json":
			var document struct {
				Schema    int                                   `json:"schema"`
				Resources map[string]map[string]LinuxPackagePin `json:"resources"`
			}
			if err := errors.Join(once(p.LinuxPackages != nil), platform.Decode(data, &document)); err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
			if document.Schema != 1 || len(document.Resources) != 1 {
				return fmt.Errorf("%s: invalid native application pin catalog", file)
			}
			for id := range document.Resources {
				if err := owner(id); err != nil {
					return err
				}
			}
			p.LinuxPackages = document.Resources
		case "buildtools-pin.json":
			var pin WindowsBuildToolsPin
			if err := errors.Join(once(p.BuildTools != nil), owner("tool.compiler"), platform.Decode(data, &pin)); err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
			p.BuildTools = &pin
		case "vcredist-pin.json":
			var pin WindowsVendorPin
			if err := errors.Join(once(p.VCRedist != nil), owner("tool.vcredist"), platform.Decode(data, &pin)); err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
			p.VCRedist = &pin
		case "windows-app-pins.json":
			pins := map[string]WindowsAppPin{}
			if err := errors.Join(once(p.WindowsApps != nil), platform.Decode(data, &pins)); err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
			for id := range pins {
				if err := owner(id); err != nil {
					return err
				}
			}
			p.WindowsApps = pins
		default:
			return fmt.Errorf("%s: unknown file in a tool folder", file)
		}
	}
	return nil
}

func (p Pins) effective(key string, pin ArchivePin) (ArchivePin, error) {
	if pin.Format == "dmg" {
		pin.DMGRevision = 1
	}
	if pin.Latex2text == nil && pin.Yamllint == nil {
		return pin, nil
	}
	python, ok := p.Archives["tool.python"][key]
	if !ok {
		return pin, fmt.Errorf("a Python tool on %s requires its Python archive", key)
	}
	if pin.Latex2text != nil {
		recipe := *pin.Latex2text
		recipe.PythonPinID, recipe.ConsoleRevision = ArchivePinID(python), 1
		pin.Latex2text = &recipe
	}
	if pin.Yamllint != nil {
		bound, err := bindYamllintPython(pin, python, key)
		if err != nil {
			return pin, err
		}
		pin = bound
	}
	if key == "windows/amd64" {
		pin.WindowsVenvRevision = 1
	}
	return pin, nil
}

func (p Pins) validate(c *Catalog) error {
	for family, platforms := range p.Archives {
		r, found := c.Resource(family)
		if !found || len(platforms) == 0 {
			return fmt.Errorf("archive family %s has no resource or no platform pin", family)
		}
		for key, pin := range platforms {
			parts := strings.Split(key, "/")
			if len(parts) == 3 && parts[0] == "linux" && distributionReleasePin.MatchString(parts[2]) {
				if _, ok := platforms[parts[0]+"/"+parts[1]]; !ok {
					return fmt.Errorf("archive %s on %s lacks its common platform pin", family, key)
				}
			} else if len(parts) != 2 {
				return fmt.Errorf("invalid archive platform %s", key)
			}
			ctx := platform.Context{OS: parts[0], Arch: parts[1]}
			if err := ctx.Validate(); err != nil {
				return fmt.Errorf("invalid archive platform %s: %w", key, err)
			}
			if !r.Available(ctx) || r.Bindings[ctx.OS].Provider != "archive" {
				return fmt.Errorf("archive %s on %s has no archive binding of its resource", family, key)
			}
			bound, err := p.effective(key, pin)
			if err == nil {
				err = bound.Validate()
			}
			if err != nil {
				return fmt.Errorf("archive %s on %s: %w", family, key, err)
			}
		}
	}
	for architecture, pin := range p.LinuxCode {
		if err := pin.Validate(); err != nil || architecture != pin.Architecture {
			return errors.Join(errors.New("invalid Linux Code platform pin"), err)
		}
		if _, declared := p.LinuxCodeModes[pin.Version]; !declared {
			return errors.New("Linux Code pin " + pin.Version + " lacks its declared modes")
		}
	}
	for version, modes := range p.LinuxCodeModes {
		if !linuxCodeVersion.MatchString(version) || modes.Sandbox != "755" && modes.Sandbox != "4755" || modes.RootOnly != "" && !linuxCodeRootOnly.MatchString(modes.RootOnly) {
			return errors.New("invalid Linux Code declared modes")
		}
	}
	for id, pins := range p.LinuxPackages {
		for architecture, pin := range pins {
			if err := pin.Validate(); err != nil || id != "tool."+pin.Package || architecture != pin.Architecture {
				return errors.Join(errors.New("invalid native application platform pin"), err)
			}
		}
	}
	if p.BuildTools != nil {
		if err := p.BuildTools.Validate(); err != nil {
			return err
		}
	}
	if p.VCRedist != nil {
		if err := p.VCRedist.Validate(); err != nil {
			return err
		}
	}
	for id, pin := range p.WindowsApps {
		if id != "app.t3-code-windows" {
			return fmt.Errorf("unsupported Windows application %s", id)
		}
		if err := pin.Validate(); err != nil {
			return err
		}
	}
	for id, missing := range map[string]bool{
		"tool.vscode":         p.LinuxCode == nil,
		"tool.tailscale":      p.LinuxPackages["tool.tailscale"] == nil,
		"tool.compiler":       p.BuildTools == nil,
		"tool.vcredist":       p.VCRedist == nil,
		"app.t3-code-windows": len(p.WindowsApps) == 0,
	} {
		if _, found := c.Resource(id); found && missing {
			return fmt.Errorf("the resource %s has no pin file in its tool folder", id)
		}
	}
	return nil
}

// Archive returns the pin of a family for the host: the pin of the
// distribution release when the family has one, else the common pin of the
// system and architecture. It returns ErrNoArchive when the family has no pin
// for the host or the pin needs another libc. The result has the recipe
// values that the file leaves to setup: the DMG revision, the Python identity
// of a Python tool and the Windows environment revision.
func (p Pins) Archive(family string, h platform.Host) (ArchivePin, error) {
	if err := h.Context.Validate(); err != nil {
		return ArchivePin{}, err
	}
	platforms := p.Archives[family]
	key := h.OS + "/" + h.Arch + "/" + h.Distro + "-" + h.DistroRelease
	pin, ok := platforms[key]
	if !ok {
		key = h.OS + "/" + h.Arch
		pin, ok = platforms[key]
	}
	if !ok || pin.Libc != "" && pin.Libc != h.Libc {
		return ArchivePin{}, fmt.Errorf("%s on %s/%s: %w", family, h.OS, h.Arch, ErrNoArchive)
	}
	return p.effective(key, pin)
}

// LinuxCodePin returns the Code DEB pin of an architecture.
func (p Pins) LinuxCodePin(arch string) (LinuxCodePin, error) {
	pin, ok := p.LinuxCode[arch]
	if !ok {
		return pin, errors.New("unsupported Linux Code architecture")
	}
	return pin, nil
}

// LinuxCodeDeclaredModes returns the declared modes of a Code version. A
// version without a declaration has a sandbox of mode 755.
func (p Pins) LinuxCodeDeclaredModes(version string) LinuxCodeModes {
	if modes, ok := p.LinuxCodeModes[version]; ok {
		return modes
	}
	return LinuxCodeModes{Sandbox: "755"}
}

// LinuxPackage returns the vendor DEB pin of a resource on an architecture.
func (p Pins) LinuxPackage(resource, arch string) (LinuxPackagePin, bool) {
	pin, ok := p.LinuxPackages[resource][arch]
	return pin, ok
}

// WindowsApp returns the installer pin of a Windows application resource.
func (p Pins) WindowsApp(resource string) (WindowsAppPin, bool) {
	pin, ok := p.WindowsApps[resource]
	return pin, ok
}
