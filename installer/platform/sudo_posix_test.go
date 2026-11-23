//go:build !windows

package platform_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

func fakeSudo(t *testing.T, exit int) (string, string) {
	t.Helper()
	directory := t.TempDir()
	calls := filepath.Join(directory, "calls")
	program := filepath.Join(directory, "sudo")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + calls + "'\necho 'sudo: a password is required' >&2\nexit " + map[bool]string{true: "0", false: "1"}[exit == 0] + "\n"
	if err := os.WriteFile(program, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return program, calls
}

func TestKeepSudoAliveRefreshesWithoutPromptingUntilRelease(t *testing.T) {
	program, calls := fakeSudo(t, 0)
	authenticated := 0
	release, err := platform.KeepSudoAliveAt(context.Background(), program, func(context.Context) error { authenticated++; return nil }, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, _ := os.ReadFile(calls)
		if strings.Count(string(data), "-n -v\n") >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("credential was not refreshed", string(data))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := release(); err != nil || authenticated != 1 {
		t.Fatal("foreground authentication repeated or release failed", authenticated, err)
	}
	before, _ := os.ReadFile(calls)
	time.Sleep(100 * time.Millisecond)
	after, _ := os.ReadFile(calls)
	if string(before) != string(after) || strings.Contains(string(after), "-v -p") {
		t.Fatal("refresh continued after release or prompted", string(after))
	}
}

func TestKeepSudoAliveReportsAStoppedRefresh(t *testing.T) {
	program, _ := fakeSudo(t, 1)
	display := &lockedBuffer{}
	release, err := platform.KeepSudoAliveAt(platform.WithOutput(context.Background(), display), program, func(context.Context) error { return nil }, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(display.String(), "credential refresh stopped") {
		if time.Now().After(deadline) {
			t.Fatal("stopped refresh was not reported", display.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := release(); err != nil || !strings.Contains(display.String(), "a password is required") {
		t.Fatal(display.String(), err)
	}
}

func TestKeepSudoAliveStopsOnFailedAuthentication(t *testing.T) {
	program, calls := fakeSudo(t, 0)
	if _, err := platform.KeepSudoAlive(context.Background(), program, func(context.Context) error { return os.ErrPermission }); err == nil {
		t.Fatal("failed authentication was accepted")
	}
	if _, err := os.Lstat(calls); !os.IsNotExist(err) {
		t.Fatal("refresh ran after a failed authentication", err)
	}
}
