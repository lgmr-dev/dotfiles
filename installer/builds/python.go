package builds

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/platform"
)

type boundedOutput struct {
	data bytes.Buffer
}

func (output *boundedOutput) Write(data []byte) (int, error) {
	const limit = 1 << 20
	remaining := limit - output.data.Len()
	if len(data) > remaining {
		n, err := output.data.Write(data[:remaining])
		return n, errors.Join(fmt.Errorf("native command output exceeds %d bytes", limit), err)
	}
	return output.data.Write(data)
}

func preparePython(ctx context.Context, pin catalog.ArchivePin, payload string) error {
	if pin.PythonStdlib == "" {
		return nil
	}
	command := pin.Commands["python3"]
	if command == "" {
		command = pin.Commands["python"]
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	args := pythonCompileArguments(filepath.Join(payload, pin.PythonStdlib), pin.PythonStdlib)
	cmd := exec.CommandContext(ctx, filepath.Join(payload, filepath.FromSlash(command)), args...)
	cmd.Dir = payload
	cmd.WaitDelay = 2 * time.Second
	var output boundedOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("prepare verified Python bytecode: %w\n%s", err, output.data.String())
	}
	return nil
}

func pythonCompileArguments(physical, recorded string) []string {
	args := []string{"-I", "-B", "-m", "compileall", "-q", "-f", "-j", "1", "--invalidation-mode", "checked-hash", "-o", "0", "-o", "1", "-o", "2"}
	if runtime.GOOS == "windows" {
		return append(args, "-d", recorded, platform.WindowsExtendedPath(physical))
	}
	return append(args, recorded)
}
