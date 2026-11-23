package builds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/lgmr-dev/dotfiles/installer/catalog"
	"github.com/lgmr-dev/dotfiles/installer/fetch"
	"github.com/lgmr-dev/dotfiles/installer/platform"
)

// The fixed upstream self-extractor needs its documented post-install step:
// https://github.com/git-for-windows/build-extra/blob/main/portable/root/README.portable
func (p *Provider) preparePortableGit(ctx context.Context, pin catalog.ArchivePin, payload string) error {
	if pin.PortableGitRevision != 1 {
		return errors.New("the PortableGit pin predates long-path support; use a checkout with the current pins")
	}
	if p.Runner == nil {
		return errors.New("PortableGit preparation requires a command runner")
	}
	program := filepath.Join(payload, pin.File)
	file, err := os.Open(program)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, io.LimitReader(file, catalog.MaxArchiveBytes+1))
	if err := errors.Join(copyErr, file.Close()); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != pin.SHA256 {
		return errors.New("PortableGit self-extractor changed after download verification")
	}
	home, temporary := filepath.Join(payload, ".prepare-home"), filepath.Join(payload, ".prepare-temp")
	for _, path := range []string{home, temporary} {
		if err := os.Mkdir(path, 0700); err != nil {
			return err
		}
	}
	environment := portableGitPreparationEnvironment(payload, home, temporary)
	run := func(phase, program string, commandEnvironment []string, args ...string) error {
		if err := p.run(ctx, program, commandEnvironment, args...); err != nil {
			return fmt.Errorf("PortableGit %s: %w", phase, err)
		}
		return nil
	}
	outputPath := payload
	if runtime.GOOS == "windows" {
		outputPath = platform.WindowsExtendedPath(payload)
	}
	if err := run("extract", program, environment, "-y", "-o"+outputPath); err != nil {
		return err
	}
	var stagedScript string
	var stagedInfo os.FileInfo
	var stagedHash [sha256.Size]byte
	if info, err := os.Lstat(filepath.Join(payload, "post-install.bat")); err == nil {
		if !info.Mode().IsRegular() || info.Size() > 64<<10 {
			return errors.New("PortableGit post-install script is not a bounded regular file")
		}
		source, err := os.Open(filepath.Join(payload, "post-install.bat"))
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(source, (64<<10)+1))
		if err := errors.Join(readErr, source.Close()); err != nil {
			return err
		}
		if len(data) > 64<<10 {
			return errors.New("PortableGit post-install script exceeds its size limit")
		}
		stagedScript = filepath.Join(payload, ".prepare-post-install.bat")
		staged, err := os.OpenFile(stagedScript, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, writeErr := staged.Write(data)
		stagedInfo, err = staged.Stat()
		if err := errors.Join(writeErr, err, staged.Close()); err != nil {
			return err
		}
		stagedHash = sha256.Sum256(data)
		if err := run("post-install", filepath.Join(payload, "git-bash.exe"), environment, "--no-needs-console", "--hide", "--cd="+payload, "--command=.prepare-post-install.bat"); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, name := range []string{"post-install.bat", "etc/post-install"} {
		if _, err := os.Lstat(filepath.Join(payload, filepath.FromSlash(name))); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(errors.New("PortableGit post-install has not completed"), err)
		}
	}
	root, err := os.OpenRoot(payload)
	if err != nil {
		return err
	}
	if err := errors.Join(fetch.CheckPayload(root, pin), root.Close()); err != nil {
		return err
	}
	if err := run("long-paths", filepath.Join(payload, "cmd", "git.exe"), environment, "-c", "core.longpaths=true", "config", "--file", filepath.Join(payload, "etc", "gitconfig"), "core.longpaths", "true"); err != nil {
		return err
	}
	if err := run("runtime", filepath.Join(payload, "bin", "bash.exe"), environment, "--noprofile", "--norc", "-p", "-c", portableGitRuntimeProbe, "dotfiles-portable-git", pin.Version); err != nil {
		return err
	}
	credentialEnvironment := slices.Clone(environment)
	for index, value := range credentialEnvironment {
		if strings.HasPrefix(value, "TMP=") {
			credentialEnvironment[index] = "TMP=" + platform.WindowsExtendedPath(temporary)
		} else if strings.HasPrefix(value, "TEMP=") {
			credentialEnvironment[index] = "TEMP=" + platform.WindowsExtendedPath(temporary)
		}
	}
	if err := run("credential-runtime", filepath.Join(payload, "ucrt64", "bin", "git-credential-manager.exe"), credentialEnvironment, "--version"); err != nil {
		return err
	}
	if stagedScript != "" {
		current, err := os.Lstat(stagedScript)
		if err != nil || !current.Mode().IsRegular() || !os.SameFile(stagedInfo, current) || current.Size() != stagedInfo.Size() {
			return errors.Join(errors.New("PortableGit post-install execution copy changed"), err)
		}
		data, err := os.ReadFile(stagedScript)
		if err != nil || sha256.Sum256(data) != stagedHash {
			return errors.Join(errors.New("PortableGit post-install execution copy changed"), err)
		}
		if err := os.Remove(stagedScript); err != nil {
			return err
		}
	}
	return os.Remove(program)
}

func portableGitPreparationEnvironment(payload, home, temporary string) []string {
	return []string{
		"HOME=" + home, "USERPROFILE=" + home, "APPDATA=" + home, "LOCALAPPDATA=" + home,
		"TEMP=" + temporary, "TMP=" + temporary, "TMPDIR=" + temporary,
		"PATH=" + strings.Join([]string{filepath.Join(payload, "cmd"), filepath.Join(payload, "bin"), filepath.Join(payload, "ucrt64", "bin"), filepath.Join(payload, "usr", "bin"), filepath.Join(os.Getenv("SystemRoot"), "System32")}, ";"),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_COUNT=0", "GIT_CONFIG_PARAMETERS=", "GIT_EXEC_PATH=" + filepath.Join(payload, "ucrt64", "libexec", "git-core"),
		"GIT_TEMPLATE_DIR=" + filepath.Join(payload, "ucrt64", "share", "git-core", "templates"),
		"BASH_ENV=", "ENV=", "CDPATH=", "MSYSTEM=UCRT64", "MSYS2_PATH_TYPE=strict", "MSYS2_ENV_CONV_EXCL=", "MSYS_NO_PATHCONV=",
		"MSYS=winsymlinks:sys",
	}
}

const portableGitRuntimeProbe = `set -eu
export PATH=/ucrt64/bin:/usr/bin:/bin
printf '%s\n' 'PortableGit runtime: prepared filesystem' >&2
test -d /dev/shm && test -d /dev/mqueue
test "$(readlink /etc/mtab)" = /proc/mounts
test -f /ucrt64/libexec/git-core/dlls-copied
printf '%s\n' 'PortableGit runtime: Git version' >&2
actual_version=$(git --version)
printf 'PortableGit runtime: actual=%s expected=git version %s\n' "$actual_version" "$1" >&2
test "$actual_version" = "git version $1"
printf '%s\n' 'PortableGit runtime: private core.longpaths' >&2
native_config=$(cygpath -w /etc/gitconfig)
actual_longpaths=$(git -c core.longpaths=true config --file "$native_config" --type=bool --get core.longpaths)
printf 'PortableGit runtime: core.longpaths=%s\n' "$actual_longpaths" >&2
test "$actual_longpaths" = true
printf '%s\n' 'PortableGit runtime: Git LFS' >&2
git lfs version >/dev/null
printf '%s\n' 'PortableGit runtime: SSH' >&2
ssh -V
printf '%s\n' portable-git-ready
`
