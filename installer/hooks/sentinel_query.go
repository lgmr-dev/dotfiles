package hooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

const sentinelQueryBound = 1 << 20

type sentinelBoundedOutput struct {
	data     bytes.Buffer
	exceeded bool
}

func (output *sentinelBoundedOutput) Write(data []byte) (int, error) {
	n := len(data)
	if remaining := sentinelQueryBound - output.data.Len(); n > remaining {
		data, output.exceeded = data[:remaining], true
	}
	output.data.Write(data)
	return n, nil
}

func sentinelQueryRun(ctx context.Context, c platform.Command, limit time.Duration, separate bool) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	process := exec.CommandContext(ctx, c.Path, c.Args...)
	process.Env = append(os.Environ(), c.Env...)
	process.Dir = c.Dir
	process.Stdin = c.Stdin
	if c.Stdin == nil {
		process.Stdin = bytes.NewReader(nil)
	}
	var output, diagnostic sentinelBoundedOutput
	process.Stdout, process.Stderr = &output, &output
	if separate {
		process.Stderr = &diagnostic
	}
	err := process.Run()
	if output.exceeded || diagnostic.exceeded || output.data.Len()+diagnostic.data.Len() > sentinelQueryBound {
		err = errors.Join(err, errors.New("native inspection exceeds 1 MiB"))
	}
	if err == nil {
		return output.data.Bytes(), nil
	}
	shown := output.data.Bytes()
	if separate {
		shown = diagnostic.data.Bytes()
	}
	failure := &platform.CommandError{Message: fmt.Sprintf("%s failed: %v%s", filepath.Base(c.Path), errors.Join(err, ctx.Err()), platform.Diagnostic(shown))}
	if process.ProcessState != nil {
		code := process.ProcessState.ExitCode()
		failure.ExitCode = &code
	}
	return output.data.Bytes(), failure
}

// SentinelQuery runs a read-only command of the Sentinel provider with a
// limit of 2 minutes. It returns at most 1 MiB of the combined output and
// keeps the exit code in the error.
func SentinelQuery(ctx context.Context, c platform.Command) ([]byte, error) {
	return sentinelQueryRun(ctx, c, 2*time.Minute, false)
}
