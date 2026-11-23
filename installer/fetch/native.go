package fetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lgmr-dev/dotfiles/installer/platform"
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

func readNativeTar(ctx context.Context, file *os.File, decoder string, arguments []string, readTar func(io.Reader) error) error {
	if !filepath.IsAbs(decoder) || filepath.Clean(decoder) != decoder || strings.ContainsAny(decoder, "\x00\r\n") {
		return errors.New("native archive requires an absolute decoder path")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, decoder, arguments...)
	command.Stdin = file
	command.Dir = filepath.Dir(file.Name())
	command.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "HOME=" + command.Dir, "TMPDIR=" + command.Dir}
	command.WaitDelay = 2 * time.Second
	var diagnostic boundedOutput
	command.Stderr = &diagnostic
	stream, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	if err := command.Start(); err != nil {
		return errors.Join(err, stream.Close())
	}
	readErr := readTar(stream)
	if readErr != nil {
		cancel()
	}
	closeErr := stream.Close()
	waitErr := command.Wait()
	if waitErr != nil {
		waitErr = fmt.Errorf("decode verified native archive: %w%s", waitErr, platform.Diagnostic(diagnostic.data.Bytes()))
	}
	return errors.Join(readErr, closeErr, waitErr)
}
