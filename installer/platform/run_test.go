package platform_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

type lockedBuffer struct {
	mutex sync.Mutex
	data  bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.data.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.data.String()
}

func shell(t *testing.T, script string) platform.Command {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fixture commands use the POSIX shell")
	}
	return platform.Command{Path: "/bin/sh", Args: []string{"-c", script}}
}

func TestRunnerStreamsOutputAndReturnsIt(t *testing.T) {
	display := &lockedBuffer{}
	ctx := platform.WithOutput(context.Background(), display)
	command := shell(t, `printf 'out:%s\n' "$FIXTURE"; cat; printf 'err\n' >&2`)
	command.Env = []string{"FIXTURE=value"}
	command.Stdin = strings.NewReader("input\n")
	command.Dir = t.TempDir()
	output, err := platform.Exec{}.Run(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"out:value\n", "input\n", "err\n"} {
		if !strings.Contains(string(output), want) || !strings.Contains(display.String(), want) {
			t.Fatal("missing output", want, string(output), display.String())
		}
	}
	if !strings.HasPrefix(display.String(), "$ sh -c ") {
		t.Fatal("the command line was not shown", display.String())
	}
}

func TestRunnerReportsTheExitCodeAndOutputTail(t *testing.T) {
	_, err := platform.Exec{}.Run(context.Background(), shell(t, `printf 'cause\033[31m\n'; exit 7`))
	code, ok := platform.ExitCode(err)
	if err == nil || !ok || code != 7 {
		t.Fatal("exit code is not data", code, ok, err)
	}
	if !strings.Contains(err.Error(), "native command failed") || !strings.Contains(err.Error(), "cause") || strings.Contains(err.Error(), "\033") {
		t.Fatal("failure lacks its sanitized output", err)
	}
	if code, ok := platform.ExitCode(nil); ok || code != 0 {
		t.Fatal("success has an exit code error")
	}
}

func TestRunnerRefusesAnInvalidCommandAndACancelledContext(t *testing.T) {
	for _, command := range []platform.Command{{Path: "sh"}, {Path: "/bin/../bin/sh"}, {Path: "/bin/sh", Env: []string{"NOVALUE"}}, {Path: "/bin/sh", Args: []string{"a\x00b"}}} {
		if _, err := (platform.Exec{}).Run(context.Background(), command); err == nil {
			t.Error("accepted", command)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	marker := filepath.Join(t.TempDir(), "marker")
	if _, err := (platform.Exec{}).Run(ctx, shell(t, "touch '"+marker+"'")); err == nil {
		t.Fatal("ran a command for a cancelled context")
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatal("command ran", err)
	}
}

func TestDisplayCommandHidesTheEnvironmentAndShortensLongLines(t *testing.T) {
	line := platform.DisplayCommand(platform.Command{Path: "/usr/bin/apt-get", Args: []string{"install", "two words"}, Env: []string{"SECRET=1"}})
	if line != `apt-get install "two words"` {
		t.Fatal(line)
	}
	long := platform.DisplayCommand(platform.Command{Path: `C:\Windows\powershell.exe`, Args: []string{strings.Repeat("x", 500)}})
	if !strings.HasPrefix(long, "powershell.exe ") || len([]rune(long)) != 241 || !strings.HasSuffix(long, "…") {
		t.Fatal(long)
	}
}

func TestDiagnosticKeepsTheTail(t *testing.T) {
	if platform.Diagnostic([]byte(" \n")) != "" {
		t.Fatal("empty output produced a diagnostic")
	}
	text := platform.Diagnostic(append(bytes.Repeat([]byte("a"), 9000), []byte("\nlast line")...))
	if !strings.HasPrefix(text, "\n[last 8 KiB of captured output]\n") || !strings.HasSuffix(text, "last line") {
		t.Fatal(text[:60])
	}
}

func TestDownloadProgressAndFormats(t *testing.T) {
	var display bytes.Buffer
	progress := &platform.DownloadProgress{Output: &display, Name: "tool", Total: 4 << 20}
	if n, err := progress.Write(make([]byte, 1<<20)); n != 1<<20 || err != nil {
		t.Fatal(n, err)
	}
	if display.String() != "\rDownloading tool: 25% of 4.0 MiB" {
		t.Fatal(display.String())
	}
	if platform.FormatBytes(1536) != "1.5 KiB" || platform.FormatElapsed(1234*time.Millisecond) != "1.2s" || platform.FormatElapsed(61500*time.Millisecond) != "1m2s" {
		t.Fatal("format changed")
	}
}
