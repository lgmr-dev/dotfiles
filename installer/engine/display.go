package engine

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

// Request is one run. Mode is "apply", "update" or "check". SelectSet is true
// when Selected is an explicit selection, also an empty one; without it Apply
// uses the saved selection. A nil Keep uses the saved kept resources.
// RemoveShared names the shared resources that Apply can release. Replace
// names the resources whose changed content the operator approves to replace;
// without it, changed content stays. Update and Check always use the saved
// selection, and Check accepts no Replace.
type Request struct {
	Mode         string
	Selected     []string
	Keep         []string
	RemoveShared []string
	Replace      []string
	SelectSet    bool
}

// Operation is the planned action for one resource: install, update, repair,
// remove, forget, keep, retain or pending. Forget releases the records of a
// resource that is already absent. Pending needs an action of the operator.
// Finding is the approved inspection. Commands are the command blocks to show
// with the operation.
type Operation struct {
	Resource   string
	Action     string
	Reason     string
	RequiredBy []string
	Privileged bool
	Commands   []CommandBlock
	Finding    Finding
}

// Plan is the preview of a run. Pending lists the pending marks of the state:
// a mutating run stops while one stays. V2Restore is the v2_restore value of
// the state: "switching" while the import from release v2 is not complete,
// "open" while restore-v2 is available.
type Plan struct {
	Mode         string
	Source       string
	Target       string
	Context      platform.Context
	Selected     []string
	Keep         []string
	RemoveShared []string
	Replace      []string
	Pending      []string
	V2Restore    string
	Operations   []Operation
}

// Result reports a run. Status is "ready", "needs-action", "failed" or
// "cancelled". Log names the apply log, when one was written.
type Result struct {
	Status  string
	Plan    Plan
	Message string
	Log     string
}

// ErrCancelled ends the interaction without a change.
var ErrCancelled = errors.New("installer interaction cancelled")

// ErrBack leaves the current screen for the main menu. It wraps ErrCancelled,
// so a flow without a menu treats it as a cancellation.
var ErrBack = fmt.Errorf("%w: back to the main menu", ErrCancelled)

// ErrPlanChanged means that the machine or the selection differs from the
// approved plan. The command line exits with 3 and the menu shows a new
// review.
var ErrPlanChanged = errors.New("machine or selection changed; review a fresh plan before applying")

// ErrReplan means that a step made a provider available and the pass stopped
// after it. The command line exits with 3 and the menu shows the remaining
// plan.
var ErrReplan = errors.New("a provider became available; run the same command again to review the remaining work")

// ErrSwitching means that the import from release v2 published the new state
// and did not complete the switch. Only the import and restore-v2 can run.
var ErrSwitching = errors.New("the import from release v2 is not complete; run setup to complete it, or run restore-v2")

// ErrPending means that the state has a pending mark. A run without the menu
// stops before any change.
var ErrPending = errors.New("an earlier step did not report its end")

// ReportedError marks a failure that the interaction already showed with its
// result, so an entry point can fail without a second print.
type ReportedError struct{ Err error }

func (e *ReportedError) Error() string { return e.Err.Error() }

// Unwrap returns the reported failure.
func (e *ReportedError) Unwrap() error { return e.Err }

// Choice is one row of a menu.
type Choice struct{ ID, Label, Detail, Category string }

// ToolMenu is the tool selection screen. Initial holds the draft or saved
// selection and Installed the saved one. Status can become ready while the
// screen is open; only the calling goroutine reads it.
type ToolMenu struct {
	Title     string
	Choices   []Choice
	Initial   []string
	Installed []string
	Status    func() ToolStatus
}

// ToolStatus describes the saved tools for the selection screen. Tools maps
// a menu entry to "needs action", "needs repair" or "update available".
type ToolStatus struct {
	Ready   bool
	Summary string
	Tools   map[string]string
}

// ReviewChoice is an optional decision on the confirmation screen. A toggle
// revises the whole proposal.
type ReviewChoice struct {
	Group, ID, Label, Detail string
	Checked                  bool
}

// Proposal is a reviewed plan, its details and its optional choices.
type Proposal struct {
	Plan    Plan
	Details string
	Notice  string
	Choices []ReviewChoice
}

// Revise computes a proposal again after the operator changes its choices.
type Revise func(context.Context, []ReviewChoice) (Proposal, error)

// ReviewKeep, ReviewReplace and ReviewShared are the groups of the review
// choices.
const (
	ReviewKeep    = "Keep retained resources"
	ReviewReplace = "Replace changed content (setup saves the current content first)"
	ReviewShared  = "Also remove unused shared dependencies"
)

// Interaction is the input and output boundary. Each method restores the
// terminal input before it returns. Review shows a proposal and calls revise,
// when it is not nil, after each change of a choice. BeginApply releases the
// screen before the approved changes run, in particular before elevation or a
// provider prompt, and returns the context that carries the progress display.
type Interaction interface {
	Choose(ctx context.Context, title string, choices []Choice, initial []string, multiple bool) ([]string, error)
	ChooseTools(ctx context.Context, menu ToolMenu) ([]string, error)
	Review(ctx context.Context, c Catalog, proposal Proposal, revise Revise) (bool, error)
	BeginApply(ctx context.Context) (context.Context, error)
	Report(Result) error
}

// ExistingPreservedPaths returns the paths that still exist.
func ExistingPreservedPaths(paths []string) []string {
	kept := []string{}
	for _, path := range paths {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		kept = append(kept, path)
	}
	return kept
}
