package hooks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

// NvimRuntimeDirectory returns the folder of the public runtime link of
// Neovim: dotfiles-runtime in the data folder of Neovim. The data folder of
// Neovim does not follow its configuration folder, and on Windows its name
// is nvim-data also with XDG_DATA_HOME set (Neovim src/nvim/os/stdpaths.c).
func NvimRuntimeDirectory(target platform.Context, folders platform.Folders) (string, error) {
	data, name := folders.Data, "nvim"
	if target.OS == "windows" {
		name = "nvim-data"
		if data == "" {
			data = folders.LocalAppData
		}
	} else if data == "" {
		data = filepath.Join(folders.Home, ".local", "share")
	}
	if !filepath.IsAbs(data) || filepath.Clean(data) != data || strings.ContainsAny(data, "\x00\r\n") {
		return "", errors.New("Neovim data folder must be an absolute canonical path")
	}
	return filepath.Join(data, name, "dotfiles-runtime"), nil
}

// NvimQuery runs the read-only verification of a Neovim runtime with a limit
// of 3 minutes and a bound of 1 MiB on its output.
func NvimQuery(ctx context.Context, c platform.Command) ([]byte, error) {
	return sentinelQueryRun(ctx, c, 3*time.Minute, false)
}

func nvimWithCompilerEnvironment(ctx context.Context, discover func(context.Context) (map[string]string, error), c platform.Command) (platform.Command, error) {
	if discover == nil {
		return c, nil
	}
	environment, err := discover(ctx)
	if err != nil {
		return c, err
	}
	basePath := ""
	for _, entry := range c.Env {
		if path, ok := strings.CutPrefix(entry, "PATH="); ok {
			basePath = path
		}
	}
	keys := []string{}
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	c.Env = slices.Clone(c.Env)
	for _, key := range keys {
		value := environment[key]
		if key == "PATH" && basePath != "" {
			value += string(os.PathListSeparator) + basePath
		}
		c.Env = append(c.Env, key+"="+value)
	}
	return c, nil
}
