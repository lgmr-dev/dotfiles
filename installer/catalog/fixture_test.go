package catalog_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/lgmr-dev/dotfiles/installer/catalog"
	"github.com/lgmr-dev/dotfiles/installer/platform"
	"github.com/lgmr-dev/dotfiles/installer/tools"
)

var (
	darwin   = platform.Context{OS: "darwin", Arch: "arm64"}
	linux    = platform.Context{OS: "linux", Arch: "amd64"}
	linuxARM = platform.Context{OS: "linux", Arch: "arm64"}
	windows  = platform.Context{OS: "windows", Arch: "amd64"}
	contexts = []platform.Context{darwin, linux, linuxARM, windows}
)

func released(t *testing.T) *catalog.Catalog {
	t.Helper()
	c, err := catalog.Load(tools.FS)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func releasedFiles(t *testing.T) fstest.MapFS {
	t.Helper()
	files := fstest.MapFS{}
	err := fs.WalkDir(tools.FS, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := fs.ReadFile(tools.FS, name)
		files[name] = &fstest.MapFile{Data: data}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func edit(t *testing.T, files fstest.MapFS, name string, change func(document map[string]any)) {
	t.Helper()
	document := map[string]any{}
	if err := json.Unmarshal(files[name].Data, &document); err != nil {
		t.Fatal(name, err)
	}
	change(document)
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	files[name] = &fstest.MapFile{Data: data}
}

func at(document any, keys ...any) map[string]any {
	for _, key := range keys {
		switch typed := key.(type) {
		case string:
			document = document.(map[string]any)[typed]
		case int:
			document = document.([]any)[typed]
		}
	}
	return document.(map[string]any)
}

func folders(t *testing.T, content map[string]catalog.Tool) fstest.MapFS {
	t.Helper()
	files := fstest.MapFS{}
	for name, tool := range content {
		tool.Schema = 1
		data, err := json.Marshal(tool)
		if err != nil {
			t.Fatal(err)
		}
		files[name+"/tool.json"] = &fstest.MapFile{Data: data}
	}
	return files
}

func one(resources ...catalog.Resource) map[string]catalog.Tool {
	return map[string]catalog.Tool{"fixture": {Resources: resources}}
}

func previous(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func decodePrevious(t *testing.T, name string, target any) {
	t.Helper()
	if err := json.Unmarshal(previous(t, name), target); err != nil {
		t.Fatal(name, err)
	}
}
