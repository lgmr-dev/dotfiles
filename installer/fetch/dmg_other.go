//go:build !darwin

package fetch

import (
	"context"
	"errors"
	"io"
	"os"
)

func readDMGArchive(context.Context, *os.File, string, func(io.Reader) error) error {
	return errors.New("disk-image application extraction requires macOS")
}
