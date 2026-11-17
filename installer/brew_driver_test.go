package installer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func brewDriverFixture(t *testing.T) (*BrewDriver, Resource, Receipt, brewIntent) {
	t.Helper()
	root, err := resolveConfigPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	d := &BrewDriver{Directory: filepath.Join(root, "provider"), Program: program, Cellar: filepath.Join(root, "Cellar"), Packages: map[string]string{"tool.first": "first"}, Environment: slices.Clone(brewProcessControls)}
	d.Query = func(context.Context, bool, string, []byte, ...string) ([]byte, error) {
		return []byte(strings.Replace(brewInventoryFixture, `"full_name":"first",`, `"full_name":"first","linked_keg":"1.0",`, 1)), nil
	}
	d.Run = func(context.Context, nativeCommand) ([]byte, error) {
		t.Error("unexpected native mutation")
		return nil, errors.New("unexpected mutation")
	}
	resource := Resource{ID: "tool.first", Name: "First", Action: "native"}
	receipt := Receipt{OperationID: strings.Repeat("a", 64), Status: "in-progress"}
	operation, err := digest(struct {
		Operation string
		Attempt   int
	}{receipt.OperationID, 0})
	if err != nil {
		t.Fatal(err)
	}
	intent := brewIntent{Schema: 3, Resource: resource.ID, Operation: receipt.OperationID, Package: "first", Action: "install", Before: map[string]nativePackage{}}
	intent.Commands = []nativeCommand{{Operation: operation, Program: program, Arguments: []string{"install", "--formula", "first"}, Environment: append(slices.Clone(d.Environment), "HOMEBREW_INSTALL_BADGE=dotfiles-operation-"+operation)}}
	return d, resource, receipt, intent
}

func TestBrewDriverRecoveryRejectsExecutableOrBroadenedState(t *testing.T) {
	for _, change := range []string{"program", "arguments", "environment", "input", "operation", "missing-command", "removal-set", "removal-bypass"} {
		t.Run(change, func(t *testing.T) {
			d, _, receipt, intent := brewDriverFixture(t)
			switch change {
			case "program":
				intent.Commands[0].Program = filepath.Join(d.Directory, "another-executable")
			case "arguments":
				intent.Commands[0].Arguments = []string{"install", "--formula", "unapproved"}
			case "environment":
				intent.Commands[0].Environment = append(intent.Commands[0].Environment, "HOMEBREW_NO_INSTALL_CLEANUP=0")
			case "input":
				intent.Commands[0].Input = []byte("input")
			case "operation":
				intent.Commands[0].Operation = strings.Repeat("b", 64)
			case "missing-command":
				intent.Commands = nil
			case "removal-set", "removal-bypass":
				intent.Action = "remove"
				intent.Remove = []string{"first"}
				intent.Commands[0].Arguments = []string{"uninstall", "--formula", "--force", "unapproved"}
				if change == "removal-bypass" {
					intent.Commands[0].Arguments = []string{"uninstall", "--formula", "--force", "--ignore-dependencies", "first"}
				}
			}
			if err := saveDocument(d.intentPath(receipt.OperationID), intent); err != nil {
				t.Fatal(err)
			}
			if _, err := d.intent(receipt.OperationID); err == nil {
				t.Fatal("saved data changed native execution authority")
			}
		})
	}
}

func TestBrewFormulaSchema3CompletionRemainsReadable(t *testing.T) {
	d, resource, receipt := brewMutationFixture(t, nil)
	if _, err := d.Apply(context.Background(), resource, Operation{Action: "install"}, receipt); err != nil {
		t.Fatal(err)
	}
	intent, err := d.intent(receipt.OperationID)
	if err != nil || intent.Schema != 5 || !slices.Contains(intent.Commands[0].Environment, "HOMEBREW_NO_EMOJI=") {
		t.Fatal("new formula intent did not bind its output controls", intent, err)
	}
	command := intent.Commands[0]
	reply, err := d.commandResult(command)
	if err != nil {
		t.Fatal(err)
	}
	// Recreate the historical persisted shape, including its original command
	// hash. Loading it must not silently add the new output control.
	intent.Schema = 3
	intent.Commands[0].Environment = slices.DeleteFunc(command.Environment, func(v string) bool { return v == "HOMEBREW_NO_EMOJI=" })
	hash, err := digest(intent.Commands[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := saveDocument(filepath.Join(d.Directory, "worker", "commands", command.Operation+".json"), nativeCommandRecord{Schema: 1, Command: hash, Reply: &reply}); err != nil {
		t.Fatal(err)
	}
	if err := saveDocument(d.intentPath(intent.Operation), intent); err != nil {
		t.Fatal(err)
	}
	observed, err := d.Observe(context.Background(), resource, receipt)
	if err != nil || !observed.Healthy || observed.CompletedOperation != intent.Operation {
		t.Fatal("historical formula completion was invalidated", observed, err)
	}
}

func TestBrewDriverPreexistingPackageCannotAcquireOwnership(t *testing.T) {
	d, r, receipt, _ := brewDriverFixture(t)
	if _, err := d.Apply(context.Background(), r, Operation{Action: "install"}, receipt); err == nil || !strings.Contains(err.Error(), "appeared") {
		t.Fatal("pre-existing package was implicitly acquired", err)
	}
	if _, err := os.Stat(d.Directory); !os.IsNotExist(err) {
		t.Fatal("refusal initialized provider state", err)
	}
}

func TestBrewDriverCompletionFlagRequiresActualCommandEvidence(t *testing.T) {
	d, r, receipt, intent := brewDriverFixture(t)
	intent.Complete = true
	if err := saveDocument(d.intentPath(receipt.OperationID), intent); err != nil {
		t.Fatal(err)
	}
	state := brewLedger{Schema: 1, Cellar: d.Cellar, Roots: map[string]brewOwnership{r.ID: {Name: "first", Source: "first", CreatedBy: receipt.OperationID}}, Pool: map[string]brewOwnership{}}
	if err := saveDocument(filepath.Join(d.Directory, "packages.json"), state); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Observe(context.Background(), r, receipt); err == nil || !strings.Contains(err.Error(), "completion") {
		t.Fatal("completion flag replaced native transaction proof", err)
	}
}

func TestBrewDriverRequiresPreservationControlsAndRejectsInheritedSecrets(t *testing.T) {
	for _, environment := range [][]string{{"LC_ALL=C"}, append(slices.Clone(brewProcessControls), "EXAMPLE_API_TOKEN=fixture-only"), append(slices.Clone(brewProcessControls), "HOMEBREW_NO_INSTALLED_DEPENDENTS_CHECK=1"), append(slices.Clone(brewProcessControls), "HOMEBREW_NO_INSTALLED_DEPENDENTS_CHECK")} {
		d, r, _, _ := brewDriverFixture(t)
		d.Environment = environment
		if _, err := d.validate(r); err == nil {
			t.Fatal("unsafe native command environment was accepted")
		}
	}
}

func TestBrewRepairRelinksAKegThatReinstallKeepsUnlinked(t *testing.T) {
	for _, mode := range []string{"relink", "conflict", "schema-4-resume"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			var installed map[string]nativePackage
			d, r, receipt := brewMutationFixture(t, func(current map[string]nativePackage) { installed = current })
			if _, err := d.Apply(ctx, r, Operation{Action: "install"}, receipt); err != nil {
				t.Fatal(err)
			}
			first := installed["first"]
			first.Healthy = false
			installed["first"] = first
			var commands [][]string
			d.Run = func(_ context.Context, command nativeCommand) ([]byte, error) {
				commands = append(commands, command.Arguments)
				output, code := fmt.Sprintf("dotfiles-operation-%s  %s/first/1.0: fixture\n", command.Operation, filepath.ToSlash(d.Cellar)), 0
				if command.Arguments[0] == "link" && mode == "conflict" {
					output, code = "Error: Could not symlink bin/first\nTarget /opt/homebrew/bin/first\nalready exists.\n", 1
				} else if command.Arguments[0] == "link" {
					output = "Linking first... 1 symlinks created.\n"
					first.Healthy = true
					installed["first"] = first
				}
				reply := nativeReply{Output: []byte(output), ExitCode: &code}
				if code != 0 {
					reply.Error = "exit status 1"
				}
				hash, err := digest(command)
				if err == nil {
					err = saveDocument(filepath.Join(d.Directory, "worker", "commands", command.Operation+".json"), nativeCommandRecord{Schema: 1, Command: hash, Reply: &reply})
				}
				if err == nil && code != 0 {
					err = reply.failure()
				}
				return reply.Output, err
			}
			repair := Receipt{OperationID: strings.Repeat("b", 64), Status: "in-progress"}
			reinstall := []string{"reinstall", "--formula", "first"}
			if mode == "schema-4-resume" {
				before, err := brewInventory(ctx, d.Query)
				if err != nil {
					t.Fatal(err)
				}
				operation, err := digest(struct {
					Operation string
					Attempt   int
				}{repair.OperationID, 0})
				if err != nil {
					t.Fatal(err)
				}
				command := nativeCommand{Operation: operation, Program: d.Program, Arguments: reinstall, Environment: d.commandEnvironment(r.ID, operation, 4)}
				intent := brewIntent{Schema: 4, Resource: r.ID, Operation: repair.OperationID, Package: "first", Action: "repair", Before: before, Commands: []nativeCommand{command}}
				if err := saveDocument(d.intentPath(repair.OperationID), intent); err != nil {
					t.Fatal(err)
				}
				if _, err := d.Run(ctx, command); err != nil {
					t.Fatal(err)
				}
				_, err = d.ResumeResource(ctx, r, Operation{Action: "repair"}, repair)
				saved, readErr := d.intent(repair.OperationID)
				if err == nil || readErr != nil || len(saved.Commands) != 1 || len(commands) != 1 {
					t.Fatal("resume changed the commands of a schema 4 repair", err, readErr, saved.Commands, commands)
				}
				return
			}
			observed, err := d.Apply(ctx, r, Operation{Action: "repair"}, repair)
			t.Logf("repair commands %v: healthy=%t err=%v", commands, observed.Healthy, err)
			if !slices.EqualFunc(commands, [][]string{reinstall, {"link", "--formula", "first"}}, slices.Equal) {
				t.Fatal("repair did not link the keg that reinstall kept unlinked", commands)
			}
			if mode == "relink" && (err != nil || !observed.Healthy) {
				t.Fatal("repair did not end with a linked keg", observed, err)
			}
			if mode == "conflict" && (err == nil || !strings.Contains(err.Error(), "already exists")) {
				t.Fatal("a link conflict did not fail the repair", err)
			}
		})
	}
}
