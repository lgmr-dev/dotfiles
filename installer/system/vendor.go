package system

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lgmr-dev/dotfiles/installer/catalog"
	"github.com/lgmr-dev/dotfiles/installer/engine"
	"github.com/lgmr-dev/dotfiles/installer/platform"
	"github.com/lgmr-dev/dotfiles/installer/state"
)

// VendorQuery runs a read-only inspection program with input on its standard
// input and returns its standard output. It never elevates.
type VendorQuery func(ctx context.Context, program string, input []byte, arguments ...string) ([]byte, error)

// VendorFetch downloads the reviewed installer at url to the new file
// destination. The provider checks the size and the SHA-256 of the file
// before it runs it.
type VendorFetch func(ctx context.Context, url, sha256 string, size int64, destination string) error

const (
	vendorSystemKey      = "windows"
	vendorNotStartedExit = 61223
	vendorDeniedExit     = 1223
)

type vendorBoundedOutput struct {
	data  bytes.Buffer
	limit int
}

func (output *vendorBoundedOutput) Write(data []byte) (int, error) {
	limit := output.limit
	if limit == 0 {
		limit = 1 << 20
	}
	remaining := limit - output.data.Len()
	if len(data) > remaining {
		n, err := output.data.Write(data[:remaining])
		return n, errors.Join(fmt.Errorf("native command output exceeds %d bytes", limit), err)
	}
	return output.data.Write(data)
}

// QueryWindowsVendor is the VendorQuery of the VC++ runtime and the Windows
// applications. It stops the inspection after 30 seconds and keeps the
// standard error stream out of the result.
func QueryWindowsVendor(ctx context.Context, program string, input []byte, arguments ...string) ([]byte, error) {
	if !filepath.IsAbs(program) || filepath.Clean(program) != program {
		return nil, errors.New("Windows vendor inspection cannot use a relative executable")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, program, arguments...)
	command.Stdin = bytes.NewReader(input)
	output, diagnostic := vendorBoundedOutput{limit: 1 << 20}, vendorBoundedOutput{}
	command.Stdout, command.Stderr = &output, &diagnostic
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("Windows vendor inspection failed: %w%s", err, platform.Diagnostic(diagnostic.data.Bytes()))
	}
	return output.data.Bytes(), nil
}

func vendorDigest(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

func vendorCompareVersions(a, b string) int {
	left, right := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(left), len(right)) {
		var x, y uint64
		if i < len(left) {
			x, _ = strconv.ParseUint(left[i], 10, 64)
		}
		if i < len(right) {
			y, _ = strconv.ParseUint(right[i], 10, 64)
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func vendorOneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func vendorPowerShellLiteral(text string) string {
	return "'" + strings.ReplaceAll(text, "'", "''") + "'"
}

func vendorCanonical(paths ...string) bool {
	for _, path := range paths {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == filepath.Dir(path) || strings.ContainsAny(path, "\x00\r\n") {
			return false
		}
	}
	return true
}

func vendorUnredirected(directories ...string) error {
	for _, directory := range directories {
		path := filepath.Join(directory, "boundary.json")
		bound, err := platform.BindDestination(path)
		if err != nil || bound != path {
			return errors.Join(fmt.Errorf("the folder %s was redirected; restore it, then run setup again", directory), err)
		}
	}
	return nil
}

func vendorInstallerPath(root, sha string) string {
	return filepath.Join(root, "payloads", sha, "installer.exe")
}

func vendorInstallerReady(file, sha string, size int64) error {
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		return errors.Join(errors.New("vendor installer payload is missing, redirected or damaged"), err)
	}
	handle, err := os.Open(file)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, readErr := io.Copy(hash, io.LimitReader(handle, size+1))
	if err := errors.Join(readErr, handle.Close()); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != sha {
		return errors.New("vendor installer payload differs from its reviewed SHA-256")
	}
	return nil
}

func vendorPrepareInstaller(ctx context.Context, fetch VendorFetch, root, source, sha string, size int64) (result error) {
	file := vendorInstallerPath(root, sha)
	directory := filepath.Dir(file)
	bound, err := platform.BindDestination(file)
	if err != nil || bound != file {
		return errors.Join(errors.New("vendor payload parent was redirected"), err)
	}
	if _, err := os.Lstat(file); errors.Is(err, os.ErrNotExist) {
		if fetch == nil {
			return errors.New("the vendor installer needs its download route")
		}
		if err := os.MkdirAll(filepath.Dir(directory), 0700); err != nil {
			return err
		}
		stage, err := os.MkdirTemp(filepath.Dir(directory), ".vendor-download-")
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, os.RemoveAll(stage)) }()
		staged := filepath.Join(stage, "installer.exe")
		if err := fetch(ctx, source, sha, size, staged); err != nil {
			return err
		}
		if err := vendorInstallerReady(staged, sha, size); err != nil {
			return err
		}
		if err := os.Rename(stage, directory); err != nil {
			return err
		}
	}
	return vendorInstallerReady(file, sha, size)
}

func vendorRun(ctx context.Context, run platform.Runner, powershell, script string, input any, elevated bool) (code int, started bool, err error) {
	data, err := json.Marshal(input)
	if err != nil {
		return 0, false, err
	}
	_, err = run.Run(ctx, platform.Command{Path: powershell, Args: windowsVendorArguments(script), Stdin: bytes.NewReader(data), Elevated: elevated})
	if err == nil {
		return 0, true, nil
	}
	code, exited := platform.ExitCode(err)
	if !exited {
		return 0, false, err
	}
	return code, code != vendorDeniedExit && code != vendorNotStartedExit, err
}

func vendorSucceeded(code int) bool {
	return code == 0 || code == 3010 || code == 1641
}

func vendorExitMessage(product string, code int, log string) string {
	switch code {
	case vendorDeniedExit, 1602:
		return product + " setup was cancelled or UAC permission was denied; no completion was claimed"
	case vendorNotStartedExit:
		return product + " setup did not start: a check before the installer failed"
	case 740:
		return product + " setup requires administrator approval"
	case 1618, 1001, 1003:
		return "another Windows installer is running; let it finish before retrying"
	case 1638:
		return product + " setup declined the change: another version of the product is installed; setup does not downgrade it"
	}
	message := fmt.Sprintf("%s setup failed with exit code %d", product, code)
	if log != "" {
		message += "; inspect " + log
	}
	return message
}

func vendorFailure(product string, code int, log string, cause error) error {
	failure := &platform.CommandError{Message: vendorExitMessage(product, code, log), ExitCode: &code}
	if cause == nil {
		return failure
	}
	return fmt.Errorf("%w: %s", failure, vendorOneLine(cause.Error()))
}

func vendorRecordName(r catalog.Resource) string {
	if binding := r.Bindings["windows"]; binding.Provider == "windows-app" {
		return binding.Package
	}
	return r.ID
}

func vendorChange(rec engine.Records, r catalog.Resource, action string, start func() (int, bool, error), verify func(code int, cause error) error) error {
	name, mark := vendorRecordName(r), r.ID+":"+action
	added := !rec.State().HasSystem(vendorSystemKey, name)
	if err := rec.Update(func(s *state.State) error {
		s.Mark(mark)
		s.AddSystem(vendorSystemKey, name)
		return nil
	}); err != nil {
		return err
	}
	code, started, cause := start()
	if !started {
		release := rec.Update(func(s *state.State) error {
			s.Clear(mark)
			if added {
				s.RemoveSystem(vendorSystemKey, name)
			}
			return nil
		})
		if cause == nil {
			cause = errors.New("the vendor installer did not start")
		}
		if code != 0 {
			cause = vendorFailure(r.Name, code, "", cause)
		}
		return errors.Join(cause, release)
	}
	if err := verify(code, cause); err != nil {
		return err
	}
	return rec.Update(func(s *state.State) error {
		s.Clear(mark)
		return nil
	})
}

func vendorForget(rec engine.Records, r catalog.Resource, present bool) error {
	name := vendorRecordName(r)
	if present || !rec.State().HasSystem(vendorSystemKey, name) {
		return nil
	}
	return rec.Update(func(s *state.State) error {
		s.RemoveSystem(vendorSystemKey, name)
		return nil
	})
}

func vendorUnknown(ctx context.Context, product string, err error) (engine.Finding, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return engine.Finding{}, ctxErr
	}
	return engine.Finding{Status: engine.Unknown, Reason: product + " could not be inspected: " + vendorOneLine(err.Error())}, nil
}
