//go:build !windows

package system

import (
	"context"
	"errors"
)

// QueryWindowsBuildTools is the VendorQuery of the Build Tools provider. It
// fails on each system other than Windows.
func QueryWindowsBuildTools(context.Context, string, []byte, ...string) ([]byte, error) {
	return nil, errors.New("Build Tools inspection requires native Windows")
}
