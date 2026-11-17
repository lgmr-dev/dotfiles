//go:build darwin || linux

package installer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLinuxCodeHealthReportsExactNativeMismatch(t *testing.T) {
	for _, mismatch := range []string{"mode", "parent", "owner", "link", "healthy"} {
		t.Run(mismatch, func(t *testing.T) {
			bin := t.TempDir()
			for name, text := range map[string]string{
				"readlink": "#!/bin/sh\nif [ \"$MISMATCH\" = link ]; then printf '/personal/code\\n'; else printf '/usr/share/code/bin/code\\n'; fi\n",
				"stat":     "#!/bin/sh\ncase \"$2\" in\n%u) if [ \"$MISMATCH:$3\" = owner:/usr/share/code ]; then printf '1001\\n'; else printf '0\\n'; fi;;\n%a) if [ \"$MISMATCH:$3\" = mode:/usr/share/code ]; then printf '775\\n'; elif [ \"$MISMATCH:$3\" = parent:/usr/share ]; then printf '777\\n'; elif [ \"$3\" = /usr/share/code/chrome-sandbox ]; then printf '4755\\n'; else printf '755\\n'; fi;;\nesac\n",
			} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(text), 0755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("/bin/sh", "-c", linuxCodeHealth)
			cmd.Env = []string{"PATH=" + bin, "MISMATCH=" + mismatch}
			out, err := cmd.CombinedOutput()
			if mismatch == "healthy" {
				if err != nil || len(out) != 0 {
					t.Fatal("healthy native query changed behavior", string(out), err)
				}
				return
			}
			want := "/usr/share/code"
			if mismatch == "link" {
				want = "/usr/bin/code target=/personal/code"
			} else if mismatch == "parent" {
				want = "/usr/share owner=0 mode=777"
			} else if mismatch == "mode" {
				want += " owner=0 mode=775"
			} else {
				want += " owner=1001 mode=755"
			}
			if err == nil || !strings.Contains(string(out), want) || len(out) > 1024 {
				t.Fatal("native mismatch lost its bounded path/metadata diagnostic", string(out), err)
			}
		})
	}
}

func TestLinuxCodeHealthFollowsVendorPackageModes(t *testing.T) {
	const sandbox = "/usr/share/code/chrome-sandbox"
	const cache = "/usr/share/code/resources/app/node-compile-cache/agent-host/v24.22.1-x64-7034790f/"
	rootOnly := []string{cache + "0a1b2c3d", cache + "4e5f6a7b"}
	for _, test := range []struct {
		name, verify, changed, meta, want string
	}{
		{name: "vendor"},
		{name: "sandbox mode", changed: sandbox, meta: "0 755 -rwxr-xr-x", want: sandbox + " owner=0 mode=755 (expected owner=0 mode=4755)"},
		{name: "root-only mode", changed: rootOnly[0], meta: "0 644 -rw-r--r--", want: rootOnly[0] + " owner=0 mode=-rw-r--r--"},
		{name: "root-only owner", changed: rootOnly[1], meta: "1000 600 -rw-------", want: rootOnly[1] + " owner=1000 mode=-rw-------"},
		{name: "root-only link", changed: rootOnly[1], meta: "0 777 lrwxrwxrwx", want: rootOnly[1] + " owner=0 mode=lrwxrwxrwx"},
		{name: "unreadable outside cache", verify: "?????????   /usr/share/code/resources/app/out/main.js\n", want: "changed package files"},
		{name: "changed cache digest", verify: "??5??????   " + rootOnly[0] + "\n", want: "changed package files"},
		{name: "missing cache file", verify: "missing     " + rootOnly[0] + "\n", want: "changed package files"},
	} {
		t.Run(test.name, func(t *testing.T) {
			bin := t.TempDir()
			stat := "#!/bin/sh\nformat=$2 path=$3 meta='0 755 drwxr-xr-x'\ncase \"$path\" in\n" +
				sandbox + ") meta='0 4755 -rwsr-xr-x';;\n" +
				cache + "*) meta='0 600 -rw-------';;\nesac\n" +
				"[ \"$path\" != '" + test.changed + "' ] || meta='" + test.meta + "'\n" +
				"set -- $meta\ncase \"$format\" in\n" +
				"%u) printf '%s\\n' \"$1\";;\n%a) printf '%s\\n' \"$2\";;\n" +
				"*) printf 'owner=%s mode=%s\\n' \"$1\" \"$3\";;\nesac\n"
			for name, text := range map[string]string{
				"readlink": "#!/bin/sh\nprintf '/usr/share/code/bin/code\\n'\n",
				"stat":     stat,
			} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(text), 0755); err != nil {
					t.Fatal(err)
				}
			}
			verify := test.verify
			for _, path := range rootOnly {
				verify += "?????????   " + path + "\n"
			}
			f, r := linuxCodeFixture(t)
			f.installed["code:amd64"] = nativePackage{Name: "code:amd64", Source: "code", Version: f.driver.Code.Version, Healthy: true}
			query := f.driver.Query
			f.driver.Query = func(ctx context.Context, privileged bool, program string, input []byte, args ...string) ([]byte, error) {
				if slices.Equal(args, []string{"--verify", "code"}) {
					return []byte(verify), nil
				}
				if program == "/bin/sh" && len(args) >= 2 && args[1] == linuxCodeHealth {
					cmd := exec.CommandContext(ctx, program, args...)
					cmd.Env = []string{"PATH=" + bin}
					var stderr strings.Builder
					cmd.Stderr = &stderr
					output, err := cmd.Output()
					if err != nil {
						return output, fmt.Errorf("%w: %s", err, stderr.String())
					}
					return output, nil
				}
				return query(ctx, privileged, program, input, args...)
			}
			o, err := f.driver.Observe(context.Background(), r, Receipt{})
			if err != nil || !o.Present {
				t.Fatal(o, err)
			}
			if test.want == "" {
				if !o.Healthy || o.HealthIssue != "" {
					t.Fatal("vendor package modes were rejected: ", o.HealthIssue)
				}
				return
			}
			if o.Healthy || !strings.Contains(o.HealthIssue, test.want) {
				t.Fatal("changed package metadata was accepted: ", o.HealthIssue)
			}
		})
	}
}
