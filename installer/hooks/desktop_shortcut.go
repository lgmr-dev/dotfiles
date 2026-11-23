package hooks

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

func desktopPowerShellArguments(script string) []string {
	units := utf16.Encode([]rune("$env:PSModulePath = [IO.Path]::Combine($PSHOME, 'Modules')\n" + script))
	data := make([]byte, len(units)*2)
	for i, unit := range units {
		binary.LittleEndian.PutUint16(data[i*2:], unit)
	}
	return []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(data)}
}

func desktopPowerShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func (p *Desktop) shortcutInput(recipe desktopRecipe) string {
	data, _ := json.Marshal(struct{ Kind, Name, Command, Icon string }{"desktop-shortcut", recipe.Name, recipe.Command, recipe.Icon})
	sum := sha256.Sum256(data)
	return filepath.Join(p.StateDir, "desktop", "inputs", hex.EncodeToString(sum[:])+".lnk")
}

func (p *Desktop) prepareShortcut(ctx context.Context, recipe desktopRecipe) (result error) {
	input := p.shortcutInput(recipe)
	if _, err := os.Lstat(input); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if p.Runner == nil || !filepath.IsAbs(p.PowerShell) {
		return errors.New("shortcut preparation requires Windows PowerShell and a command runner")
	}
	if bound, err := platform.BindDestination(input); err != nil || bound != input {
		return errors.Join(errors.New("shortcut input parent was redirected"), err)
	}
	for _, folder := range []string{p.StateDir, filepath.Join(p.StateDir, "desktop"), filepath.Dir(input)} {
		if err := platform.PrepareDirectory(folder); err != nil {
			return err
		}
	}
	directory := strings.TrimSuffix(input, ".lnk") + "-" + strings.ToLower(rand.Text())
	if err := os.Mkdir(directory, 0700); err != nil {
		return err
	}
	defer func() {
		if result == nil {
			result = os.RemoveAll(directory)
		}
	}()
	stage := filepath.Join(directory, "staged.lnk")
	script := `$ErrorActionPreference='Stop'
$ProgressPreference='SilentlyContinue'
$destination=` + desktopPowerShellQuote(stage) + `
$target=` + desktopPowerShellQuote(recipe.Command) + `
$icon=` + desktopPowerShellQuote(recipe.Icon) + `
if (Test-Path -LiteralPath $destination) { throw 'Private shortcut input already exists without this preparation result; preserve it for inspection' }
$link=(New-Object -ComObject WScript.Shell).CreateShortcut($destination)
$link.TargetPath=$target; $link.Arguments=''; $link.WorkingDirectory=[IO.Path]::GetDirectoryName($target); $link.IconLocation=$icon+',0'; $link.Save()
(Get-FileHash -LiteralPath $destination -Algorithm SHA256).Hash.ToLowerInvariant()`
	output, err := p.Runner.Run(ctx, platform.Command{Path: p.PowerShell, Args: desktopPowerShellArguments(script)})
	if err != nil {
		return fmt.Errorf("prepare private Start menu shortcut: %w", err)
	}
	data, err := platform.ReadDocument(stage)
	if err != nil {
		return err
	}
	fields := strings.Fields(string(output))
	if sum := sha256.Sum256(data); len(fields) == 0 || hex.EncodeToString(sum[:]) != fields[len(fields)-1] {
		return fmt.Errorf("shortcut bytes differ from native preparation result (native output: %d bytes, PowerShell CLIXML: %t); the attempt stays at %s", len(output), strings.Contains(string(output), "#< CLIXML"), directory)
	}
	ready := filepath.Join(directory, "ready.lnk")
	file, err := os.OpenFile(ready, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	return platform.MoveNoReplace(ready, input)
}
