package terminal

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
	"golang.org/x/term"
)

func reviewTexts(lines []reviewLine) []string {
	texts := []string{}
	for _, line := range lines {
		texts = append(texts, line.text)
	}
	return texts
}

func TestReviewContainsDependencyPrivilegeAndCompleteLongReason(t *testing.T) {
	resources := &catalog.Catalog{Resources: []catalog.Resource{{ID: "compiler", Name: "Compiler", Action: "compiler", Bindings: map[string]catalog.Binding{"linux": {Provider: "apt", Package: "build-essential"}}}}}
	reason := strings.Repeat("compilers are needed for parser builds ", 10)
	kept, deleted := filepath.Join(t.TempDir(), "saved edits"), filepath.Join(t.TempDir(), "deleted copy")
	if err := os.WriteFile(kept, []byte("edits"), 0600); err != nil {
		t.Fatal(err)
	}
	plan := engine.Plan{Mode: "apply", Source: "source", Target: "linux/amd64:/fixture", Context: platform.Context{OS: "linux", Arch: "amd64"}, Pending: []string{"tool.zsh:install"}, Operations: []engine.Operation{
		{Resource: "compiler", Action: "install", Reason: reason, RequiredBy: []string{"neovim"}, Privileged: true,
			Finding: engine.Finding{Version: "13.2", Notes: []string{"run the engine setup"}, Preserved: []string{kept, deleted}}},
	}}
	lines := PlanText(resources, plan)
	for _, expected := range []string{"Operation: apply", "Target: linux/amd64:/fixture", "Source: source", "Unfinished step: tool.zsh:install", "INSTALL: Compiler (compiler)",
		"Why: " + reason, "Required by: neovim", "Provider: apt", "Installed version: 13.2", "Requires administrator privileges.",
		"Note: run the engine setup", "Preserved files for inspection: " + kept} {
		if !slices.Contains(lines, expected) {
			t.Fatal("approval omitted consequential detail", expected)
		}
	}
	if slices.Contains(lines, "Preserved files for inspection: "+deleted) {
		t.Fatal("approval listed a preserved copy that no longer exists")
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "Plan: ") || strings.HasPrefix(line, "Ownership: ") {
			t.Fatal("approval names a concept that no longer exists", line)
		}
	}
	report := ReportText(engine.Result{Status: "ready", Message: "Note: run the engine setup"})
	if !strings.Contains(report, "Notes (informational, not failures):\n  - Note: run the engine setup\n") || strings.Contains(report, "need your action") {
		t.Fatal("a note was reported as an action", report)
	}
	if strings.Join(wrapLines([]string{reason}, 31), "") != reason {
		t.Fatal("narrow preview lost approval text")
	}
}

func TestCommandBlocksStayCopyReady(t *testing.T) {
	resources := &catalog.Catalog{Resources: []catalog.Resource{
		{ID: "tool.zsh", Name: "Zsh", Action: "zsh"},
		{ID: "tool.git", Name: "Git", Action: "git"},
	}}
	long := "sudo apt-get install --no-remove " + strings.Repeat("package-with-a-long-name ", 8) + "last"
	plan := engine.Plan{Mode: "apply", Operations: []engine.Operation{
		{Resource: "tool.git", Action: "install", Reason: "required by selection", Commands: []engine.CommandBlock{
			{Title: "Install Git", Lines: []string{"sudo apt-get install \x1b[1;31mgit\x1b[0m", long}, By: "setup"}}},
		{Resource: "tool.zsh", Action: "retain", Reason: "setup never removes a system package", Commands: []engine.CommandBlock{
			{Title: "Remove Zsh", Lines: []string{"sudo apt-get remove zsh"}, By: "you"}}},
	}}
	lines := PlanText(resources, plan)
	text := strings.Join(lines, "\n")
	for _, block := range []string{
		"Install Git\nSetup runs:\n    sudo apt-get install git\n    " + long + "\n",
		"Remove Zsh\nRun these commands yourself:\n    sudo apt-get remove zsh\n",
	} {
		if !strings.Contains(text, block) {
			t.Fatalf("preview lacks the command block %q in %q", block, text)
		}
	}
	rows := layout(planText(resources, plan, ""), 40)
	copied, started := "", false
	for _, row := range rows {
		if !started && !strings.HasPrefix(row.text, "    sudo apt-get install --no-remove") {
			if row.joined {
				t.Fatal("an ordinary line was left to the terminal wrap", row.text)
			}
			continue
		}
		started = true
		copied += row.text
		if !row.joined {
			break
		}
		if len(row.text) != 40 {
			t.Fatalf("a joined row must fill the width so the terminal wraps it: %q", row.text)
		}
	}
	if copied != "    "+long {
		t.Fatalf("a copy of the wrapped command differs from the command: %q", copied)
	}
	report := ReportText(engine.Result{Status: "ready", Plan: plan, Message: "All selected resources passed verification.", Log: "/state/logs/apply.log"})
	if !strings.Contains(report, "\nRemove Zsh\nRun these commands yourself:\n    sudo apt-get remove zsh\nFull log: /state/logs/apply.log\n") {
		t.Fatalf("report lacks the commands of the operator: %q", report)
	}
	if strings.Contains(report, "Setup runs:") || strings.Contains(report, "\x1b") {
		t.Fatalf("report shows a command that setup already ran, or a control code: %q", report)
	}
	if cancelled := ReportText(engine.Result{Status: "cancelled", Plan: plan}); strings.Contains(cancelled, "apt-get") {
		t.Fatalf("a cancelled run reported commands: %q", cancelled)
	}
}

type workflowFiles struct{ directory string }

func (d workflowFiles) Inspect(_ context.Context, r catalog.Resource, want engine.Want, _ engine.Records) (engine.Finding, error) {
	data, err := os.ReadFile(filepath.Join(d.directory, r.ID))
	switch {
	case errors.Is(err, os.ErrNotExist):
		if !want.Present {
			return engine.Finding{Status: engine.Absent, Token: "absent"}, nil
		}
		return engine.Finding{Status: engine.Missing, Token: "absent", Commands: []engine.CommandBlock{
			{Title: "Install " + r.Name, Lines: []string{"fixture-install " + r.ID}, By: "setup"}}}, nil
	case err != nil:
		return engine.Finding{}, err
	case string(data) != "installed":
		return engine.Finding{Status: engine.Changed, Reason: "edited after setup wrote it", Token: string(data)}, nil
	}
	return engine.Finding{Status: engine.Satisfied, Token: "installed"}, nil
}

func (d workflowFiles) Apply(_ context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	path := filepath.Join(d.directory, r.ID)
	if approved.Replace {
		if err := os.Rename(path, path+".saved"); err != nil {
			return err
		}
	}
	if err := os.WriteFile(path, []byte("installed"), 0600); err != nil {
		return err
	}
	return rec.Update(func(s *state.State) error {
		if err := s.BeginTarget(path, state.Record{Resource: r.ID, Kind: "file", Next: "fp:1", Before: "absent"}); err != nil {
			return err
		}
		return s.Promote(path)
	})
}

func (d workflowFiles) Remove(_ context.Context, r catalog.Resource, _ engine.Finding, rec engine.Records) error {
	path := filepath.Join(d.directory, r.ID)
	if err := os.Remove(path); err != nil {
		return err
	}
	return rec.Update(func(s *state.State) error {
		s.Drop(path)
		return nil
	})
}

func testWorkflowChild(t *testing.T, mode string, before *term.State) {
	home := t.TempDir()
	files := workflowFiles{home}
	w := &Workflow{Input: os.Stdin, Output: os.Stdout}
	e := &engine.Engine{
		Catalog: &catalog.Catalog{Resources: []catalog.Resource{
			{ID: "agent", Name: "Coding agent", Category: "AI coding", Capability: true, Requires: []string{"runtime"}},
			{ID: "runtime", Name: "Shared runtime", Action: "runtime", Shared: true},
		}},
		Router:   func(catalog.Resource) (engine.Provider, error) { return files, nil },
		Context:  platform.Context{OS: runtime.GOOS, Arch: runtime.GOARCH},
		StateDir: filepath.Join(home, "ledger"),
		Target:   runtime.GOOS + "/" + runtime.GOARCH + ":" + home,
		Source:   "native-ui-fixture",
	}
	runtimeFile := filepath.Join(home, "runtime")
	changed := mode == "workflow-replace" || mode == "workflow-keep-changed"
	if mode == "workflow-shared" || changed {
		request := engine.Request{Mode: "apply", Selected: []string{"agent"}, SelectSet: true}
		plan, err := e.Preview(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if installed, err := e.Apply(context.Background(), request, plan); err != nil || installed.Status != "ready" {
			t.Fatal(installed, err)
		}
	}
	if changed {
		if err := os.WriteFile(runtimeFile, []byte("edited"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	e.UI = w
	result, err := e.Run(context.Background())
	if err := errors.Join(err, w.Close()); err != nil {
		t.Fatal(err)
	}
	saved := func() state.State {
		st, err := state.Peek(e.StateDir, e.Target)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	expected := "cancelled"
	switch mode {
	case "workflow-shared":
		expected = "ready"
		if _, statErr := os.Stat(runtimeFile); len(saved().Selected) != 0 || !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal("approved shared removal did not run", saved(), statErr)
		}
	case "workflow-replace":
		expected = "ready"
		current, currentErr := os.ReadFile(runtimeFile)
		kept, keptErr := os.ReadFile(runtimeFile + ".saved")
		if currentErr != nil || keptErr != nil || string(current) != "installed" || string(kept) != "edited" {
			t.Fatal("approved replacement did not save the changed content and apply the selected content", string(current), currentErr, string(kept), keptErr)
		}
	case "workflow-keep-changed":
		expected = "needs-action"
		if current, err := os.ReadFile(runtimeFile); err != nil || string(current) != "edited" {
			t.Fatal("changed content did not stay without an approval", string(current), err)
		}
	case "workflow-apply":
		expected = "ready"
		if st := saved(); len(st.Selected) != 1 || st.Selected[0] != "agent" || st.Targets[runtimeFile].Resource != "runtime" {
			t.Fatal("connected installation lost selection or dependency record", st)
		}
	default:
		if _, err := os.Stat(e.StateDir); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("cancelled workflow created state", err)
		}
	}
	if result.Status != expected {
		t.Fatal(result)
	}
	fmt.Println("Ready for ordinary input")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ordinary input" {
		t.Fatal("workflow stole subsequent prompt input", err)
	}
	after, err := term.GetState(int(os.Stdin.Fd()))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("workflow failed to restore terminal state", err)
	}
	fmt.Printf("workflow=%s\nrestored=true\n", result.Status)
}

func TestNativeControllerWorkflowAppliesOnlyAfterExplicitConfirmation(t *testing.T) {
	for _, mode := range []string{"apply", "back", "declined", "tools-back", "type-ahead", "double-escape", "quit"} {
		t.Run(mode, func(t *testing.T) {
			s := startNativeSession(t, "workflow-"+mode)
			s.waitFor(t, "Choose the tools you want installed")
			s.waitFor(t, "> [ ] AI coding")
			switch mode {
			case "quit":
				s.send(t, "\x03")
			case "tools-back":
				s.send(t, "\x1b")
				s.waitFor(t, "What would you like to do?")
				s.send(t, "\r")
				s.waitForCount(t, "Choose the tools you want installed", 2)
				s.send(t, "\x03")
			case "double-escape":
				s.send(t, " \r")
				s.waitFor(t, "Apply these changes?")
				s.send(t, "d")
				s.waitFor(t, "Required by: agent")
				s.send(t, "\x1b\x1b")
				s.waitFor(t, "What would you like to do?")
				s.send(t, "k\r")
			case "type-ahead":
				s.send(t, " \r\r")
				s.waitFor(t, "Apply these changes?")
				s.send(t, "\x1b")
				s.waitFor(t, "What would you like to do?")
				s.send(t, "k\r")
			default:
				s.send(t, " \r")
				s.waitFor(t, "Apply these changes?")
				s.waitFor(t, "Add: 1  Change: 0")
				s.waitFor(t, "> Apply changes")
				s.send(t, "d")
				s.waitFor(t, "Required by: agent")
				s.waitFor(t, "Setup runs:")
				s.waitFor(t, "fixture-install runtime")
				s.send(t, "\x1b")
				s.waitForCount(t, "Apply these changes?", 2)
				switch mode {
				case "apply":
					s.send(t, "\r")
					s.waitFor(t, "==> [1/1] Installing Shared runtime")
					s.waitFor(t, "==> Done: everything selected is ready.")
				case "back":
					s.send(t, "\x1b")
				case "declined":
					s.send(t, "j\r")
				}
				if mode != "apply" {
					s.waitFor(t, "What would you like to do?")
					s.send(t, "k\r")
				}
			}
			status := "cancelled"
			if mode == "apply" {
				status = "ready"
			}
			s.waitFor(t, "workflow="+status)
			s.waitFor(t, "restored=true")
		})
	}
}

func reviewScreen(screen string) bool {
	return strings.Contains(screen, "Apply these changes?") && !strings.Contains(screen, "Updating the plan...")
}

func TestNativeWorkflowRevisesSharedRemovalInOneReview(t *testing.T) {
	s := startNativeSession(t, "workflow-shared")
	s.chooseMenu(t, 0, "Choose tools")
	s.waitFor(t, "All 1 installed tools are healthy and current.")
	s.send(t, " ")
	s.waitFor(t, "Add 0, remove 1")
	s.send(t, "\r")
	screen, seen := s.nextScreen(t, 0, reviewScreen, 15*time.Second)
	if !strings.Contains(screen, "Also remove unused shared dependencies:") || !strings.Contains(screen, "> Apply changes") || !strings.Contains(screen, "Remove: 0  Keep: 1") {
		t.Fatalf("confirmation lacks the shared decision: %q", screen)
	}
	s.send(t, "k ")
	s.waitFor(t, "Updating the plan...")
	screen, seen = s.nextScreen(t, seen, func(screen string) bool {
		return reviewScreen(screen) && strings.Contains(screen, "> [x] Shared runtime")
	}, 15*time.Second)
	if !strings.Contains(screen, "Remove: 1  Keep: 0") {
		t.Fatalf("revision did not update the counts: %q", screen)
	}
	s.send(t, "j")
	s.nextScreen(t, seen, func(screen string) bool { return reviewScreen(screen) && strings.Contains(screen, "> Apply changes") }, 15*time.Second)
	s.send(t, "\r")
	s.waitFor(t, "==> [1/1] Removing Shared runtime")
	s.waitFor(t, "Full log: ")
	s.waitFor(t, "workflow=ready")
	s.waitFor(t, "restored=true")
}

func TestNativeWorkflowReplacesChangedContentOnlyAfterItsChoice(t *testing.T) {
	for _, mode := range []string{"replace", "keep-changed"} {
		t.Run(mode, func(t *testing.T) {
			s := startNativeSession(t, "workflow-"+mode)
			s.chooseMenu(t, 0, "Choose tools")
			s.waitFor(t, "1 of 1 installed tools need attention.")
			s.waitFor(t, "(needs action)")
			s.send(t, "\r")
			screen, seen := s.nextScreen(t, 0, reviewScreen, 15*time.Second)
			if !strings.Contains(screen, "Replace changed content (setup saves the current content first):") || !strings.Contains(screen, "[ ] Shared runtime") ||
				!strings.Contains(screen, "> Apply changes") || !strings.Contains(screen, "Add: 0  Change: 0") || !strings.Contains(screen, "Needs action: 1") {
				t.Fatalf("confirmation lacks the unchecked replacement decision: %q", screen)
			}
			if mode == "keep-changed" {
				s.send(t, "\r")
				s.waitFor(t, "==> Finished, but these items need your action:")
				s.waitFor(t, "workflow=needs-action")
				s.waitFor(t, "restored=true")
				return
			}
			s.send(t, "k ")
			s.waitFor(t, "Updating the plan...")
			screen, seen = s.nextScreen(t, seen, func(screen string) bool {
				return reviewScreen(screen) && strings.Contains(screen, "> [x] Shared runtime")
			}, 15*time.Second)
			if !strings.Contains(screen, "Add: 0  Change: 1") || !strings.Contains(screen, "Needs action: 0") {
				t.Fatalf("revision did not turn the kept content into a change: %q", screen)
			}
			s.send(t, "j")
			s.nextScreen(t, seen, func(screen string) bool { return reviewScreen(screen) && strings.Contains(screen, "> Apply changes") }, 15*time.Second)
			s.send(t, "\r")
			s.waitFor(t, "==> [1/1] Repairing Shared runtime")
			s.waitFor(t, "workflow=ready")
			s.waitFor(t, "restored=true")
		})
	}
}

func TestToolScreenSummarizesChangesAndLabelsStatus(t *testing.T) {
	tools := ToolScreen{Installed: []string{"git", "zsh"}, Status: func() (string, map[string]string) {
		return "1 of 2 installed tools need attention.", map[string]string{"zsh": "needs repair"}
	}}
	options := []Option{{ID: "git", Label: "Git"}, {ID: "zsh", Label: "zsh"}, {ID: "fzf", Label: "fzf"}}
	summary, labels := tools.summary(options, map[string]bool{"zsh": true, "fzf": true})
	if summary != "Add 1, remove 1 | 1 of 2 installed tools need attention." {
		t.Fatal(summary)
	}
	screen := render("Choose tools", selectionRows(options, true), map[string]bool{"zsh": true}, 0, true, 80, 20, summary, labels)
	if !strings.Contains(screen, "[x] zsh (needs repair)") || !strings.Contains(screen, summary) {
		t.Fatal("status was not shown", screen)
	}
}

func TestConfirmRowsBoundHighlightsAndEndWithActions(t *testing.T) {
	resources := []catalog.Resource{}
	operations := []engine.Operation{}
	for i := range 12 {
		id := fmt.Sprintf("tool%d", i)
		resources = append(resources, catalog.Resource{ID: id, Name: id, Action: "native"})
		operations = append(operations, engine.Operation{Resource: id, Action: "remove"})
	}
	proposal := engine.Proposal{Plan: engine.Plan{Operations: operations}, Choices: []engine.ReviewChoice{
		{Group: engine.ReviewReplace, ID: "tool1", Label: "tool1"},
		{Group: engine.ReviewShared, ID: "tool0", Label: "tool0", Checked: true},
	}}
	rows := confirmRows(&catalog.Catalog{Resources: resources}, proposal)
	texts := []string{}
	for _, row := range rows {
		texts = append(texts, row.text)
	}
	want := []string{"+5 more | d: details", "", engine.ReviewReplace + ":", "[ ] tool1", "", engine.ReviewShared + ":", "[x] tool0", "", "Apply changes", "Back to main menu"}
	if len(rows) != maxHighlights+9 || !slices.Equal(texts[maxHighlights-1:], want) || rows[len(rows)-2].action != "apply" || rows[len(rows)-1].action != "back" ||
		rows[maxHighlights+2].choice != 0 || rows[maxHighlights+5].choice != 1 || rows[maxHighlights+1].selectable() {
		t.Fatal("confirmation rows", texts)
	}
}

func TestReviewNeutralizesTerminalControlsWithoutDroppingVisibleText(t *testing.T) {
	if got := strings.Join(wrapLines([]string{"ab\x1b[2Jcd\x00ef\u0085g"}, 4), ""); got != "ab[2Jcdefg" {
		t.Fatal(got)
	}
}

func TestConfirmationNamesWhatNeedsAction(t *testing.T) {
	resources := &catalog.Catalog{Resources: []catalog.Resource{{ID: "rg", Name: "ripgrep", Action: "archive"}}}
	plan := engine.Plan{Operations: []engine.Operation{{Resource: "rg", Action: "pending", Reason: "changed content stays as it is"}}}
	if !slices.Contains(planHighlights(resources, plan), "NEEDS ACTION: ripgrep - changed content stays as it is") {
		t.Fatal(planHighlights(resources, plan))
	}
}

func TestReviewLeadsWithRemovalAndPrivilegeSummary(t *testing.T) {
	resources := &catalog.Catalog{Resources: []catalog.Resource{
		{ID: "add", Name: "New tool", Action: "add"},
		{ID: "remove", Name: "Removed tool", Action: "remove"},
		{ID: "gone", Name: "Absent tool", Action: "remove"},
	}}
	plan := engine.Plan{Operations: []engine.Operation{
		{Resource: "add", Action: "install", Privileged: true},
		{Resource: "remove", Action: "remove"},
		{Resource: "gone", Action: "forget"},
	}}
	lines := reviewTexts(planText(resources, plan, ""))
	beforeDetails := strings.Join(lines[:min(7, len(lines))], "\n")
	for _, expected := range []string{"Add: 1  Change: 0", "Remove: 2", "Admin: 1", "INSTALL: New tool", "REMOVE: Removed tool", "FORGET: Absent tool"} {
		if !strings.Contains(beforeDetails, expected) {
			t.Fatal("approval buried consequential changes", expected, beforeDetails)
		}
	}
}
