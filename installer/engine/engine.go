package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
)

// Engine runs setup for one target. StateDir is the state folder and Target
// the state target "<os>/<arch>:<home>". Source is the hash of the installer
// executable. UI is nil outside the menu. Sweep, when set, removes unused
// private builds after a pass without a failure. Elevate, when set, starts
// the administrator session before the first privileged step and returns its
// release. Selection, when set, receives the closure of the selected and kept
// resources of a request before its inspections and before the steps of its
// approved plan, so a provider whose content depends on the selection can use
// it. Leftovers, when set, returns the paths that setup keeps for the owner
// and no longer uses: a check lists them as notes. Switch is true only for
// the run of the import that completes the switch from release v2: that run
// is permitted while v2_restore is "switching", and it does not close the v2
// fallback. Each other confirmed apply or update closes an open fallback
// immediately before its first step that applies or removes; a run without
// such a step leaves the fallback open, and a step that fails without a
// change of the targets, the system records and the pending marks opens it
// again. RestoreV2, when set, is the restore
// of release v2 that asks before it replaces a changed slot: the action menu
// offers it while v2_restore is "open".
type Engine struct {
	Catalog  Catalog
	Router   Router
	Context  platform.Context
	StateDir string
	Target   string
	Source   string
	UI       Interaction
	Sweep    func(ctx context.Context, rec Records) error
	Elevate  func(ctx context.Context) (func() error, error)

	Selection func(wanted []string)
	Leftovers func(ctx context.Context) ([]string, error)
	Switch    bool
	RestoreV2 func(ctx context.Context, ui Interaction) (Result, error)
}

type viewRecords struct{ st state.State }

func (v viewRecords) State() state.State { return v.st.Clone() }

func (v viewRecords) Update(func(*state.State) error) error {
	return errors.New("an inspection only reads; it cannot change the state")
}

type storeRecords struct{ store *state.Store }

func (s storeRecords) State() state.State                         { return s.store.View() }
func (s storeRecords) Update(edit func(*state.State) error) error { return s.store.Update(edit) }

func (e *Engine) validate() error {
	if e.Catalog == nil || e.Router == nil || e.StateDir == "" || e.Target == "" || e.Source == "" {
		return errors.New("the engine requires its catalog, router, state folder, target and source")
	}
	return e.Context.Validate()
}

func (e *Engine) inspectOne(ctx context.Context, id string, want Want, rec Records) (Finding, error) {
	r, _ := e.Catalog.Resource(id)
	provider, err := e.Router(r)
	if err != nil {
		return Finding{Status: Unknown, Reason: err.Error()}, ctx.Err()
	}
	f, err := provider.Inspect(ctx, r, want, rec)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return f, ctxErr
	}
	if err != nil {
		return Finding{Status: Unknown, Reason: "inspection failed: " + err.Error()}, nil
	}
	return f, nil
}

func (e *Engine) inspect(ctx context.Context, st state.State, request Request, rec Records) (Findings, error) {
	found := Findings{Wanted: map[string]Finding{}, Released: map[string]Finding{}}
	wanted, released, err := Scope(e.Catalog, e.Context, st, request)
	if err != nil {
		return found, err
	}
	if e.Selection != nil {
		e.Selection(slices.Clone(wanted))
	}
	for _, id := range wanted {
		if r, _ := e.Catalog.Resource(id); r.Action == "" {
			continue
		}
		if found.Wanted[id], err = e.inspectOne(ctx, id, Want{Present: true, Update: request.Mode == "update"}, rec); err != nil {
			return found, err
		}
	}
	for _, id := range released {
		if found.Released[id], err = e.inspectOne(ctx, id, Want{}, rec); err != nil {
			return found, err
		}
	}
	return found, nil
}

// Preview inspects the system and returns the plan of a request. It only
// reads: it takes no lock and creates no file.
func (e *Engine) Preview(ctx context.Context, request Request) (Plan, error) {
	if err := e.validate(); err != nil {
		return Plan{}, err
	}
	st, err := state.Peek(e.StateDir, e.Target)
	if err != nil {
		return Plan{}, err
	}
	return e.PreviewState(ctx, st, request)
}

// PreviewState inspects the system and returns the plan of a request for a
// state that is not the saved one. The import from release v2 uses it for
// the state that it maps in memory. It only reads.
func (e *Engine) PreviewState(ctx context.Context, st state.State, request Request) (Plan, error) {
	if err := e.validate(); err != nil {
		return Plan{}, err
	}
	found, err := e.inspect(ctx, st, request, viewRecords{st})
	if err != nil {
		return Plan{}, err
	}
	return PlanChanges(e.Catalog, e.Context, st, request, found, e.Source)
}

// Check inspects the saved selection and reports each difference, pending
// mark, unfinished change of a target record and resource that setup cannot
// inspect. It never installs or repairs.
func (e *Engine) Check(ctx context.Context) (Result, error) {
	if err := e.validate(); err != nil {
		return Result{Status: "failed"}, err
	}
	st, err := state.Peek(e.StateDir, e.Target)
	if err != nil {
		return Result{Status: "failed"}, err
	}
	plan, err := e.PreviewState(ctx, st, Request{Mode: "check"})
	if err != nil {
		return Result{Status: "failed"}, err
	}
	return e.checkResult(ctx, st, plan), nil
}

func pendingLine(mark string) string {
	return "An earlier step did not report its end: " + mark + ". Open setup, confirm that its command exited, then choose the action again."
}

func (e *Engine) disclosures(plan Plan) string {
	preserved, notes := []string{}, []string{}
	for _, op := range plan.Operations {
		preserved = append(preserved, op.Finding.Preserved...)
		notes = append(notes, op.Finding.Notes...)
	}
	message := ""
	for _, path := range ExistingPreservedPaths(sortedUnique(preserved)) {
		message += "\nPreserved files for inspection: " + path
	}
	for _, note := range sortedUnique(notes) {
		message += "\nNote: " + note
	}
	return message
}

func (e *Engine) checkResult(ctx context.Context, st state.State, plan Plan) Result {
	reasons := []string{}
	if plan.V2Restore == "switching" {
		reasons = append(reasons, switchingLine)
	}
	for _, mark := range plan.Pending {
		reasons = append(reasons, pendingLine(mark))
	}
	for _, key := range slices.Sorted(maps.Keys(st.Targets)) {
		if record := st.Targets[key]; record.Next != "" {
			reasons = append(reasons, "unfinished change: "+key+" ("+record.Resource+"). Run setup again to finish it.")
		}
	}
	retainedNotes := []string{}
	for _, op := range plan.Operations {
		r, _ := e.Catalog.Resource(op.Resource)
		switch {
		case op.Action == "retain" && (op.Finding.Status == Unknown || op.Finding.Status == Blocked):
			reasons = append(reasons, r.Name+" ("+r.ID+"): "+withReason("setup could not inspect this resource", op.Finding.Reason))
		case op.Action == "retain" && op.Finding.Status == Changed:
			retainedNotes = append(retainedNotes, "\nNote: "+r.Name+" ("+r.ID+") stays without a selected tool and has changed content"+withReason("", op.Finding.Reason))
		case op.Action != "keep" && op.Action != "retain":
			reasons = append(reasons, r.Name+" ("+r.ID+"): "+op.Reason)
		}
	}
	status, message := "ready", "All selected resources passed verification."
	if len(reasons) > 0 {
		status, message = "needs-action", strings.Join(reasons, "\n")
	}
	message += strings.Join(retainedNotes, "") + e.disclosures(plan)
	if e.Leftovers != nil {
		left, err := e.Leftovers(ctx)
		if err != nil {
			message += "\nNote: setup could not list the content that it keeps and no longer uses: " + err.Error()
		}
		for _, path := range sortedUnique(left) {
			message += "\nNote: setup keeps " + path + " and no longer uses it; inspect it, then delete it yourself"
		}
	}
	return Result{Status: status, Plan: plan, Message: message + e.v2FolderReport(plan.V2Restore)}
}

func sameSelection(plan Plan, request Request) bool {
	return plan.Mode == request.Mode && slices.Equal(plan.Selected, request.Selected) && slices.Equal(plan.Keep, request.Keep) && slices.Equal(plan.RemoveShared, request.RemoveShared) && slices.Equal(plan.Replace, request.Replace)
}

// Apply runs the approved plan of a request. It takes the installer lock,
// stops at a pending mark and inspects each operation again before its first
// change: a different token returns ErrPlanChanged and changes nothing. Then
// it saves the selection and runs each step: inspect again and compare the
// token, apply or remove, inspect again to verify. A failed step blocks its
// dependents and the other steps continue. A different token returns
// ErrPlanChanged. A step that makes a provider available ends the pass with
// ErrReplan when work remains. After a pass without a failure it runs the
// sweep.
func (e *Engine) Apply(ctx context.Context, request Request, approved Plan) (Result, error) {
	return e.apply(ctx, request, approved, nil)
}

func (e *Engine) apply(ctx context.Context, request Request, approved Plan, confirmed []string) (result Result, resultErr error) {
	result = Result{Status: "failed", Plan: approved}
	if err := e.validate(); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if request.Mode != "apply" && request.Mode != "update" {
		return result, errors.New("a read-only request cannot apply changes")
	}
	store, err := state.Open(e.StateDir, e.Target)
	if err != nil {
		return result, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, store.Close())
		if resultErr != nil && result.Status != "needs-action" {
			result.Status = "failed"
		}
	}()
	st := store.View()
	if slices.ContainsFunc(st.Pending, func(mark string) bool { return !slices.Contains(confirmed, mark) }) {
		lines := []string{}
		for _, mark := range st.Pending {
			lines = append(lines, pendingLine(mark))
		}
		result.Status, result.Message = "needs-action", strings.Join(lines, "\n")
		return result, fmt.Errorf("%w: %s", ErrPending, strings.Join(st.Pending, ", "))
	}
	if st.V2Restore == "switching" && !e.Switch {
		result.Status, result.Message = "needs-action", switchingLine
		return result, ErrSwitching
	}
	effective, err := resolve(e.Catalog, st, request)
	if err != nil {
		return result, err
	}
	if !sameSelection(approved, effective) || approved.V2Restore != st.V2Restore {
		return result, ErrPlanChanged
	}
	if e.Selection != nil {
		wanted, err := e.Catalog.Closure(slices.Concat(effective.Selected, effective.Keep), e.Context)
		if err != nil {
			return result, err
		}
		e.Selection(wanted)
	}
	ReportStep(ctx, "Checking the reviewed plan again")
	for _, op := range approved.Operations {
		r, known := e.Catalog.Resource(op.Resource)
		if !known {
			return result, fmt.Errorf("approved plan names the unknown resource %q", op.Resource)
		}
		released := op.Action == "remove" || op.Action == "forget" || op.Action == "retain"
		want := Want{Present: !released, Update: request.Mode == "update" && !released}
		fresh, err := e.inspectOne(ctx, op.Resource, want, viewRecords{st})
		if err != nil {
			return result, err
		}
		completed := !released && fresh.Status == Satisfied && op.Finding.Status != Satisfied
		if fresh.Token != op.Finding.Token && !completed {
			result.Message = r.Name + " changed after approval; review a new plan"
			return result, ErrPlanChanged
		}
	}
	if len(st.Pending) != 0 {
		if err := store.Update(func(s *state.State) error {
			for _, mark := range st.Pending {
				s.Clear(mark)
			}
			return nil
		}); err != nil {
			return result, err
		}
	}
	closeFallback := st.V2Restore == "open" && !e.Switch
	logNotes := []string{}
	log, err := openApplyLog(e.StateDir, time.Now())
	if err != nil {
		logNotes = append(logNotes, "Apply log issue: "+err.Error())
	}
	if log != nil {
		ctx = log.attach(ctx)
	}
	defer func() {
		if log != nil {
			result.Log = log.file.Name()
			if err := log.close(); err != nil {
				logNotes = append(logNotes, "Apply log is incomplete: "+err.Error())
			}
		}
		if len(logNotes) != 0 {
			result.Message = strings.TrimSpace(result.Message + "\n" + strings.Join(logNotes, "\n"))
		}
	}()
	rec := storeRecords{store}
	if request.Mode == "apply" {
		if err := store.Update(func(s *state.State) error {
			s.Selected, s.Keep = slices.Clone(approved.Selected), slices.Clone(approved.Keep)
			return nil
		}); err != nil {
			return result, err
		}
	}
	total, step := 0, 0
	privileged := false
	for _, op := range approved.Operations {
		if mutating(op.Action) {
			total++
			privileged = privileged || op.Privileged
		}
	}
	if privileged && e.Elevate != nil {
		release, err := e.Elevate(ctx)
		if err != nil {
			return result, err
		}
		defer func() { resultErr = errors.Join(resultErr, release()) }()
	}
	blocked, held := map[string]string{}, map[string]string{}
	failures, actions := []string{}, []string{}
	for index, op := range approved.Operations {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		r, _ := e.Catalog.Resource(op.Resource)
		if op.Action == "pending" {
			if op.Finding.Status != Changed {
				blocked[r.ID] = op.Reason
			}
			actions = append(actions, r.Name+" ("+r.ID+"): "+op.Reason)
			continue
		}
		if !mutating(op.Action) {
			continue
		}
		step++
		removal := op.Action == "remove" || op.Action == "forget"
		waiting := ""
		if removal {
			waiting = held[r.ID]
		} else {
			closure, err := e.Catalog.Closure([]string{r.ID}, e.Context)
			if err != nil {
				return result, err
			}
			for _, dependency := range closure {
				if reason, stopped := blocked[dependency]; stopped && dependency != r.ID {
					waiting = "waiting for " + dependency + ": " + reason
					break
				}
			}
		}
		if waiting != "" {
			ReportStep(ctx, "[%d/%d] %s needs action: %s", step, total, r.Name, waiting)
			blocked[r.ID] = waiting
			actions = append(actions, r.Name+" ("+r.ID+"): "+waiting)
			continue
		}
		started := time.Now()
		ReportStep(ctx, "[%d/%d] %s%s", step, total, progressVerb(op.Action), r.Name)
		want := Want{Present: !removal, Update: request.Mode == "update"}
		provider, err := e.Router(r)
		var fresh Finding
		if err == nil {
			fresh, err = provider.Inspect(ctx, r, want, viewRecords{rec.State()})
		}
		if err == nil && !removal && fresh.Token != op.Finding.Token && fresh.Status == Satisfied {
			ReportStep(ctx, "[%d/%d] %s is already ready", step, total, r.Name)
			continue
		}
		if err == nil && fresh.Token != op.Finding.Token {
			ReportStep(ctx, "[%d/%d] %s changed after approval", step, total, r.Name)
			result.Message = strings.Join(append(failures, r.Name+" changed after approval; review a new plan"), "\n")
			return result, ErrPlanChanged
		}
		var closedAt *state.State
		if err == nil && closeFallback {
			err = store.Update(func(s *state.State) error { s.V2Restore = ""; return nil })
			closeFallback = false
			if err == nil {
				before := store.View()
				closedAt = &before
			}
		}
		if err == nil {
			if removal {
				err = provider.Remove(ctx, r, op.Finding, rec)
			} else {
				err = provider.Apply(ctx, r, op.Finding, rec)
			}
		}
		if err == nil {
			err = e.verify(ctx, provider, r, op, want, rec)
		}
		if err != nil {
			ReportStep(ctx, "[%d/%d] %s failed after %s", step, total, r.Name, platform.FormatElapsed(time.Since(started)))
			failure := r.Name + " (" + r.ID + "): " + err.Error()
			if after := store.View(); closedAt != nil && reflect.DeepEqual(closedAt.Targets, after.Targets) && reflect.DeepEqual(closedAt.System, after.System) && slices.Equal(closedAt.Pending, after.Pending) {
				if reopenErr := store.Update(func(s *state.State) error { s.V2Restore = "open"; return nil }); reopenErr == nil {
					closeFallback = true
					failure += "\nNothing changed, so the v2 fallback stays open."
				}
			}
			failures = append(failures, failure)
			blocked[r.ID] = "its " + op.Action + " step failed"
			if removal {
				closure, closureErr := e.Catalog.Closure([]string{r.ID}, e.Context)
				if closureErr != nil {
					return result, closureErr
				}
				for _, dependency := range closure {
					if dependency != r.ID {
						held[dependency] = r.ID + " still needs it: its removal failed"
					}
				}
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				result.Message = strings.Join(failures, "\n")
				return result, ctxErr
			}
			continue
		}
		ReportStep(ctx, "[%d/%d] %s done in %s", step, total, r.Name, platform.FormatElapsed(time.Since(started)))
		if !(r.ReplanAfter || op.Finding.Replan) || removal {
			continue
		}
		replan, err := e.laterWork(approved.Operations[index+1:], r.ID)
		if err != nil {
			return result, err
		}
		if replan {
			result.Status = "needs-action"
			result.Message = strings.TrimSpace(strings.Join(failures, "\n") + "\n" + r.Name + " is ready. Review the newly available provider inventory before continuing.")
			return result, ErrReplan
		}
	}
	if len(failures) != 0 {
		result.Message = strings.Join(append(failures, actions...), "\n") + e.disclosures(approved)
		return result, fmt.Errorf("%d of %d steps failed", len(failures), total)
	}
	if e.Sweep != nil {
		ReportStep(ctx, "Removing unused private builds")
		if err := e.Sweep(ctx, rec); err != nil {
			logNotes = append(logNotes, "Note: cleanup of unused private builds stopped: "+err.Error())
		}
	}
	if err := store.Update(func(s *state.State) error { s.Source = e.Source; return nil }); err != nil {
		return result, err
	}
	result.Status, result.Message = "ready", "All selected resources passed verification."
	if len(actions) != 0 {
		result.Status, result.Message = "needs-action", strings.Join(actions, "\n")
	}
	result.Message += e.disclosures(approved)
	return result, nil
}

func (e *Engine) laterWork(later []Operation, bootstrap string) (bool, error) {
	for _, op := range later {
		if mutating(op.Action) {
			return true, nil
		}
		if op.Action != "pending" {
			continue
		}
		closure, err := e.Catalog.Closure([]string{op.Resource}, e.Context)
		if err != nil {
			return false, err
		}
		if slices.Contains(closure, bootstrap) {
			return true, nil
		}
	}
	return false, nil
}

func (e *Engine) verify(ctx context.Context, provider Provider, r catalog.Resource, op Operation, want Want, rec Records) error {
	after, err := provider.Inspect(ctx, r, want, viewRecords{rec.State()})
	if err != nil {
		return fmt.Errorf("verification after the %s step failed: %w", op.Action, err)
	}
	if !want.Present {
		if after.Status == Unknown || after.Status == Blocked {
			return fmt.Errorf("setup could not verify the removal: %s", withReason(after.Status.String(), after.Reason))
		}
		if Recorded(rec.State(), r, e.Context) {
			return fmt.Errorf("the removal left a record: %s", withReason(after.Status.String(), after.Reason))
		}
		return nil
	}
	if after.Status == Satisfied || after.Status == Changed || after.Status == Outdated && op.Action != "update" {
		return nil
	}
	return fmt.Errorf("did not pass verification after its %s step: %s", op.Action, withReason(after.Status.String(), after.Reason))
}
