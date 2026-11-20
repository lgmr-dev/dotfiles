package engine

import (
	"context"
	"errors"
	"fmt"
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
// release.
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
	found, err := e.inspect(ctx, st, request, viewRecords{st})
	if err != nil {
		return Plan{}, err
	}
	return PlanChanges(e.Catalog, e.Context, st, request, found, e.Source)
}

// Check inspects the saved selection and reports each difference, pending
// mark and resource that setup cannot inspect. It never installs or repairs.
func (e *Engine) Check(ctx context.Context) (Result, error) {
	plan, err := e.Preview(ctx, Request{Mode: "check"})
	if err != nil {
		return Result{Status: "failed"}, err
	}
	return e.checkResult(plan), nil
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

func (e *Engine) checkResult(plan Plan) Result {
	reasons := []string{}
	for _, mark := range plan.Pending {
		reasons = append(reasons, pendingLine(mark))
	}
	for _, op := range plan.Operations {
		if op.Action != "keep" && op.Action != "retain" {
			r, _ := e.Catalog.Resource(op.Resource)
			reasons = append(reasons, r.Name+" ("+r.ID+"): "+op.Reason)
		}
	}
	status, message := "ready", "All selected resources passed verification."
	if len(reasons) > 0 {
		status, message = "needs-action", strings.Join(reasons, "\n")
	}
	return Result{Status: status, Plan: plan, Message: message + e.disclosures(plan)}
}

func sameSelection(plan Plan, request Request) bool {
	return plan.Mode == request.Mode && slices.Equal(plan.Selected, request.Selected) && slices.Equal(plan.Keep, request.Keep) && slices.Equal(plan.RemoveShared, request.RemoveShared) && slices.Equal(plan.Replace, request.Replace)
}

// Apply runs the approved plan of a request. It takes the installer lock,
// stops at a pending mark, saves the selection and runs each step: inspect
// again and compare the token, apply or remove, inspect again to verify. A
// failed step blocks its dependents and the other steps continue. A different
// token returns ErrPlanChanged. A step that makes a provider available ends
// the pass with ErrReplan when work remains. After a pass without a failure it
// runs the sweep.
func (e *Engine) Apply(ctx context.Context, request Request, approved Plan) (result Result, resultErr error) {
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
	if len(st.Pending) != 0 {
		lines := []string{}
		for _, mark := range st.Pending {
			lines = append(lines, pendingLine(mark))
		}
		result.Status, result.Message = "needs-action", strings.Join(lines, "\n")
		return result, fmt.Errorf("%w: %s", ErrPending, strings.Join(st.Pending, ", "))
	}
	effective, err := resolve(e.Catalog, st, request)
	if err != nil {
		return result, err
	}
	if !sameSelection(approved, effective) {
		return result, ErrPlanChanged
	}
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
		r, known := e.Catalog.Resource(op.Resource)
		if !known {
			return result, fmt.Errorf("approved plan names the unknown resource %q", op.Resource)
		}
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
			fresh, err = provider.Inspect(ctx, r, want, rec)
		}
		if err == nil && fresh.Token != op.Finding.Token {
			ReportStep(ctx, "[%d/%d] %s changed after approval", step, total, r.Name)
			result.Message = strings.Join(append(failures, r.Name+" changed after approval; review a new plan"), "\n")
			return result, ErrPlanChanged
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
		if (r.ReplanAfter || op.Finding.Replan) && !removal && slices.ContainsFunc(approved.Operations[index+1:], func(later Operation) bool { return mutating(later.Action) }) {
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

func (e *Engine) verify(ctx context.Context, provider Provider, r catalog.Resource, op Operation, want Want, rec Records) error {
	after, err := provider.Inspect(ctx, r, want, rec)
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
