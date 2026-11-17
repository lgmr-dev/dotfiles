//go:build darwin || linux

package installer

import (
	"context"
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
				"stat":     "#!/bin/sh\ncase \"$2\" in\n%u) if [ \"$MISMATCH:$3\" = owner:/usr/share/code ]; then printf '1001\\n'; else printf '0\\n'; fi;;\n%a) if [ \"$MISMATCH:$3\" = mode:/usr/share/code ]; then printf '775\\n'; elif [ \"$MISMATCH:$3\" = parent:/usr/share ]; then printf '777\\n'; else printf '755\\n'; fi;;\nesac\n",
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

func TestLinuxCodeCheckAcceptsOnlyTheModesThatThePackageDeclares(t *testing.T) {
	const cache = "/usr/share/code/resources/app/node-compile-cache/main/v24.22.1-x64-7034790f/0a1b2c3d"
	for name, c := range map[string]struct {
		verify, sandbox, cache string
		healthy                bool
	}{
		"plain sandbox":                 {sandbox: "755", healthy: true},
		"setuid sandbox and root cache": {verify: "?????????   " + cache + "\n", sandbox: "4755", cache: "0:0:8180", healthy: true},
		"group-writable setuid sandbox": {sandbox: "4775"},
		"setgid sandbox":                {sandbox: "6755"},
		"cache owned by a user":         {verify: "?????????   " + cache + "\n", sandbox: "4755", cache: "1000:0:8180"},
		"cache with another mode":       {verify: "?????????   " + cache + "\n", sandbox: "4755", cache: "0:0:8100"},
		"cache is a symbolic link":      {verify: "?????????   " + cache + "\n", sandbox: "4755", cache: "0:0:a1ff"},
		"unreadable file outside cache": {verify: "?????????   /usr/share/code/resources/app/out/main.js\n", sandbox: "4755", cache: "0:0:8180"},
		"changed sandbox digest":        {verify: "??5??????   /usr/share/code/chrome-sandbox\n", sandbox: "4755"},
	} {
		t.Run(name, func(t *testing.T) {
			f, r := linuxCodeFixture(t)
			f.approve()
			receipt := aptFixtureReceipt("code-modes")
			if _, err := f.driver.Apply(context.Background(), r, Operation{Action: "install"}, receipt); err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			for program, text := range map[string]string{
				"readlink": "#!/bin/sh\nprintf '/usr/share/code/bin/code\\n'\n",
				"stat":     "#!/bin/sh\ncase \"$2 $3\" in\n'%a /usr/share/code/chrome-sandbox') printf '%s\\n' \"$SANDBOX\";;\n'%u '*) printf '0\\n';;\n'%a '*) printf '755\\n';;\n\"%u:%g:%f $CACHE\") printf '%s\\n' \"$META\";;\n*) exit 1;;\nesac\n",
			} {
				if err := os.WriteFile(filepath.Join(bin, program), []byte(text), 0755); err != nil {
					t.Fatal(err)
				}
			}
			query := f.driver.Query
			f.driver.Query = func(ctx context.Context, privileged bool, program string, input []byte, args ...string) ([]byte, error) {
				if slices.Equal(args, []string{"--verify", "code"}) {
					return []byte(c.verify), nil
				}
				if program == "/bin/sh" && len(args) >= 2 && args[1] == linuxCodeHealth {
					cmd := exec.CommandContext(ctx, program, args...)
					cmd.Env = []string{"PATH=" + bin, "SANDBOX=" + c.sandbox, "CACHE=" + cache, "META=" + c.cache}
					return cmd.CombinedOutput()
				}
				return query(ctx, privileged, program, input, args...)
			}
			o, err := f.driver.Observe(context.Background(), r, receipt)
			if err != nil || o.Healthy != c.healthy || c.healthy != (o.HealthIssue == "") {
				t.Fatal("Code health differs from the modes that the package declares", o.Healthy, o.HealthIssue, err)
			}
		})
	}
}
