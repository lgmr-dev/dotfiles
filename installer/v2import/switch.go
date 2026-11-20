package v2import

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/hooks"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/targets"
)

var fault func(point string) error

func hit(point string) error {
	if fault != nil {
		return fault(point)
	}
	return nil
}

var errSwitchOnly = errors.New("this change belongs to the switch")

// Proposal is the preview of the import, or of the completion of a switch
// that an earlier run left unfinished. Plan is the plan of the closure of
// the imported selection. Details is the text for the review: the mapping
// table, the builds to make, the targets that change and the folders of
// release v2 that stay. Switching is true when the schema-2 state is
// published and the run only completes the switch.
type Proposal struct {
	Plan      engine.Plan
	Details   string
	Switching bool
	Mapping   Mapping

	o Options
}

func mutating(action string) bool {
	return action == "install" || action == "repair" || action == "update" || action == "remove" || action == "forget"
}

func buildResource(r catalog.Resource, ctx platform.Context) bool {
	return r.Bindings[ctx.OS].Provider == "archive" || r.ID == hooks.NvimResource
}

func targetResource(r catalog.Resource) bool {
	return r.Action == "config" || hooks.ProfileResource(r.ID) || hooks.DesktopResource(r.ID) || r.ID == hooks.FontResource
}

func (o Options) name(id string) string {
	if r, known := o.Engine.Catalog.Resource(id); known && r.Name != "" {
		return r.Name + " (" + id + ")"
	}
	return id
}

func (o Options) details(plan engine.Plan, mapping *Mapping, deferred []string) string {
	lines := []string{"Import from release v2 (" + SourceCommit + ", " + SourceRelease + ") into release v3."}
	section := func(title string, items []string) {
		if len(items) != 0 {
			lines = append(lines, "", title)
			for _, item := range items {
				lines = append(lines, "  "+item)
			}
		}
	}
	blockers, work := o.phases(plan)
	builds, switched, decisions := []string{}, []string{}, []string{}
	for _, op := range work {
		builds = append(builds, o.name(op.Resource))
	}
	for _, op := range plan.Operations {
		r, _ := o.Engine.Catalog.Resource(op.Resource)
		switch {
		case op.Action == "pending" && op.Finding.Status == engine.Changed:
			decisions = append(decisions, o.name(op.Resource)+": "+op.Reason)
		case mutating(op.Action) && !buildResource(r, plan.Context):
			switched = append(switched, o.name(op.Resource)+": "+op.Action)
		}
	}
	if mapping != nil {
		section("Items that need work first. The import stops before the switch, and release v2 stays active:", blockers)
	}
	section("Private tools that setup builds under "+filepath.Join(o.Engine.StateDir, "tools")+", beside the payloads of release v2:", builds)
	section("Targets that the switch changes, after each build passed its check:", switched)
	section("Content that stays as it is until your decision:", decisions)
	names := []string{}
	for _, id := range deferred {
		names = append(names, o.name(id))
	}
	section("Applications that stay on release v2 in this run. A later Choose tools or Update tools offers each cask install:", names)
	section("Folders of release v2 that stay. Setup never deletes them:", existingV2Folders(o.Engine.StateDir))
	if mapping != nil {
		rows := []string{}
		for _, row := range mapping.Rows {
			text := row.Resource
			if row.Target != "" {
				text += " -> " + row.Target
			}
			rows = append(rows, text+": "+row.Result)
		}
		section("Records of release v2:", rows)
		section("Notes:", mapping.Notes)
	}
	lines = append(lines, "", "Setup saves each target before its first change in "+restoreDir(o.Engine.StateDir)+". After the switch, restore-v2 can give release v2 back until your next confirmed change.")
	return strings.Join(lines, "\n")
}

func (o Options) phases(plan engine.Plan) (blockers []string, builds []engine.Operation) {
	for _, op := range plan.Operations {
		r, _ := o.Engine.Catalog.Resource(op.Resource)
		switch {
		case op.Action == "pending" && op.Finding.Status != engine.Changed:
			blockers = append(blockers, o.name(op.Resource)+": "+op.Reason)
		case op.Action != "install" && op.Action != "repair" && op.Action != "update", targetResource(r):
		case buildResource(r, plan.Context):
			builds = append(builds, op)
		default:
			blockers = append(blockers, o.name(op.Resource)+": "+op.Reason)
		}
	}
	return blockers, builds
}

func stagingDir(stateDir string) string { return filepath.Join(restoreDir(stateDir), "staging") }

func (o Options) staged() map[string]state.Record {
	st, err := state.Peek(stagingDir(o.Engine.StateDir), o.Engine.Target)
	records := map[string]state.Record{}
	if err != nil {
		return records
	}
	tools := filepath.Join(o.Engine.StateDir, "tools")
	for key, record := range st.Targets {
		if within(tools, key) {
			records[key] = record
		}
	}
	return records
}

// Preview maps a schema-1 state and plans the import, or plans the
// completion of an unfinished switch. It only reads. SetLegacy, when set,
// receives the command folders of the payloads of release v2 that stay
// active, before the inspection of the shell profile.
func Preview(ctx context.Context, o Options, setLegacy func(directories []string)) (*Proposal, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	e := o.Engine
	current, err := state.Peek(e.StateDir, e.Target)
	switch {
	case errors.Is(err, state.ErrSchema1):
	case err != nil:
		return nil, err
	case current.V2Restore == "switching":
		set, err := openRestoreSet(o)
		if err != nil {
			return nil, err
		}
		if setLegacy != nil {
			setLegacy(set.manifest.Legacy)
		}
		plan, err := o.switchEngine(set).Preview(ctx, engine.Request{Mode: "apply"})
		if err != nil {
			return nil, err
		}
		return &Proposal{Plan: plan, Switching: true, Details: o.details(plan, nil, set.manifest.Deferred), o: o}, nil
	default:
		return nil, errors.New("this installation has no state of release v2 to import")
	}
	mapping, err := Map(ctx, o)
	if err != nil {
		return nil, err
	}
	if setLegacy != nil {
		setLegacy(mapping.Legacy)
	}
	st := mapping.State.Clone()
	for key, record := range o.staged() {
		st.Targets[key] = record
	}
	preview := *e
	preview.Catalog, preview.UI = deferCatalog{e.Catalog, mapping.Deferred}, nil
	plan, err := preview.PreviewState(ctx, st, engine.Request{Mode: "apply"})
	if err != nil {
		return nil, err
	}
	for _, op := range plan.Operations {
		for key, record := range st.Targets {
			if op.Action == "remove" && record.Resource == op.Resource && record.Applied != "" {
				return nil, fmt.Errorf("the import would remove %s of %s, which release v2 owns. The import never removes a target: keep release v2 (%s) and report this plan. Nothing was changed", key, op.Resource, SourceCommit)
			}
		}
	}
	return &Proposal{Plan: plan, Mapping: mapping, Details: o.details(plan, &mapping, mapping.Deferred), o: o}, nil
}

type guarded struct {
	store *state.Store
	tools string
}

func (g guarded) State() state.State { return g.store.View() }

func (g guarded) Update(edit func(*state.State) error) error {
	return g.store.Update(func(s *state.State) error {
		before := s.Clone()
		if err := edit(s); err != nil {
			return err
		}
		for _, key := range changedTargets(before, *s) {
			if !within(g.tools, key) {
				return fmt.Errorf("%s: %w", key, errSwitchOnly)
			}
		}
		if !reflect.DeepEqual(before.System, s.System) || !slices.Equal(before.Pending, s.Pending) {
			return errSwitchOnly
		}
		return nil
	})
}

func needsWork(status engine.Status) bool {
	return status != engine.Satisfied && status != engine.Outdated && status != engine.Changed
}

func (p *Proposal) build(ctx context.Context, st *state.State) error {
	e := p.o.Engine
	blockers, builds := p.o.phases(p.Plan)
	if len(blockers) != 0 {
		return fmt.Errorf("the import stopped before the switch, and release v2 stays active. These items need work first:\n  %s\nRepair them with the setup of %s (%s), then run this setup again", strings.Join(blockers, "\n  "), SourceCommit, SourceRelease)
	}
	staging := stagingDir(e.StateDir)
	if err := platform.PrepareDirectory(staging); err != nil {
		return err
	}
	store, err := state.Open(staging, e.Target)
	if err != nil {
		return err
	}
	fail := func(err error) error { return errors.Join(err, store.Close()) }
	if err := store.Update(func(s *state.State) error {
		s.Selected, s.Keep, s.System, s.Targets, s.V2Restore = st.Selected, st.Keep, st.System, st.Targets, ""
		return nil
	}); err != nil {
		return fail(err)
	}
	rec := guarded{store, filepath.Join(e.StateDir, "tools")}
	for index, op := range builds {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		r, _ := e.Catalog.Resource(op.Resource)
		stopped := func(err error) error {
			return fail(fmt.Errorf("the import stopped before the switch, and release v2 stays active. %s did not build: %w", p.o.name(r.ID), err))
		}
		provider, err := e.Router(r)
		if err != nil || provider == nil {
			return stopped(errors.Join(errors.New("it has no provider"), err))
		}
		finding, err := provider.Inspect(ctx, r, engine.Want{Present: true}, rec)
		if err != nil {
			return stopped(err)
		}
		if !needsWork(finding.Status) {
			continue
		}
		engine.ReportStep(ctx, "[build %d/%d] %s", index+1, len(builds), r.Name)
		err = provider.Apply(ctx, r, finding, rec)
		if errors.Is(err, errSwitchOnly) || errors.Is(err, targets.ErrChanged) {
			continue
		}
		if err != nil {
			return stopped(err)
		}
		after, err := provider.Inspect(ctx, r, engine.Want{Present: true}, rec)
		if err != nil || needsWork(after.Status) {
			return stopped(errors.Join(fmt.Errorf("its check after the build gave %s: %s", after.Status, after.Reason), err))
		}
	}
	for key, record := range store.View().Targets {
		if within(rec.tools, key) {
			st.Targets[key] = record
		}
	}
	return store.Close()
}

func encodeState(st state.State) ([]byte, error) {
	st = st.Clone()
	st.Schema = 2
	st.Selected, st.Keep, st.Pending = sortedUnique(st.Selected), sortedUnique(st.Keep), nil
	for provider, names := range st.System {
		st.System[provider] = sortedUnique(names)
	}
	if err := st.Validate(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return nil, err
	}
	var decoded state.State
	if err := platform.Decode(data, &decoded); err != nil {
		return nil, err
	}
	return append(data, '\n'), decoded.Validate()
}

func (p *Proposal) publish(ctx context.Context) (resultErr error) {
	e := p.o.Engine
	release, err := platform.Lock(filepath.Join(e.StateDir, state.LockFile))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, release()) }()
	current, err := platform.ReadDocument(filepath.Join(e.StateDir, state.File))
	if err != nil || !bytes.Equal(current, p.Mapping.Original) {
		return errors.Join(fmt.Errorf("the state of release v2 changed after the review: %w", engine.ErrPlanChanged), err)
	}
	st := p.Mapping.State.Clone()
	for key, record := range p.o.staged() {
		st.Targets[key] = record
	}
	if err := p.build(ctx, &st); err != nil {
		return err
	}
	st.V2Restore = "switching"
	data, err := encodeState(st)
	if err != nil {
		return err
	}
	if err := hit("restore-set"); err != nil {
		return err
	}
	if err := createRestoreSet(p.o, p.Mapping, st); err != nil {
		return err
	}
	if err := hit("publish"); err != nil {
		return err
	}
	return platform.SaveDocument(filepath.Join(e.StateDir, state.File), data)
}

func covered(approved, next engine.Plan) bool {
	for _, op := range next.Operations {
		if !mutating(op.Action) {
			continue
		}
		removal := op.Action == "remove" || op.Action == "forget"
		if !slices.ContainsFunc(approved.Operations, func(earlier engine.Operation) bool {
			return earlier.Resource == op.Resource && mutating(earlier.Action) && removal == (earlier.Action == "remove" || earlier.Action == "forget")
		}) {
			return false
		}
	}
	return true
}

func (p *Proposal) complete(ctx context.Context) (engine.Result, error) {
	o := p.o
	result := engine.Result{Status: "failed", Plan: p.Plan}
	set, err := openRestoreSet(o)
	if err != nil {
		return result, err
	}
	e := o.switchEngine(set)
	request := engine.Request{Mode: "apply"}
	plan := p.Plan
	if !p.Switching {
		if plan, err = e.Preview(ctx, request); err != nil {
			return result, err
		}
		if !covered(p.Plan, plan) {
			return result, fmt.Errorf("the remaining switch has a change that the review did not show: %w", engine.ErrPlanChanged)
		}
	}
	result, err = e.Apply(ctx, request, plan)
	if err != nil {
		return result, err
	}
	unfinished := []string{}
	for _, op := range plan.Operations {
		if op.Action == "pending" && op.Finding.Status != engine.Changed {
			unfinished = append(unfinished, o.name(op.Resource)+": "+op.Reason)
		}
	}
	if len(unfinished) != 0 {
		result.Status = "needs-action"
		result.Message = strings.TrimSpace(result.Message + "\nThe switch from release v2 is not complete. Resolve these items and run setup again, or run restore-v2:\n  " + strings.Join(unfinished, "\n  "))
		return result, nil
	}
	if o.VerifyNvim != nil && slices.ContainsFunc(plan.Operations, func(op engine.Operation) bool { return op.Resource == hooks.NvimResource }) {
		if err := o.VerifyNvim(ctx); err != nil {
			result.Status = "failed"
			return result, fmt.Errorf("Neovim does not start through the switched runtime link; the switch stays open. Run setup again, or run restore-v2: %w", err)
		}
	}
	if err := hit("open"); err != nil {
		return result, err
	}
	store, err := state.Open(e.StateDir, e.Target)
	if err != nil {
		return result, err
	}
	err = store.Update(func(s *state.State) error { s.V2Restore = "open"; return nil })
	if err = errors.Join(err, store.Close()); err != nil {
		return result, err
	}
	result.Plan.V2Restore = "open"
	result.Message = strings.TrimSpace(result.Message + "\nThe import from release v2 is complete. restore-v2 can give release v2 back until your next confirmed change. Keep the folders of release v2 until then.")
	return result, nil
}

// Run does the approved import: it builds the private tools beside release
// v2, stops before the switch when a build fails or a system item needs
// work, saves the restore set, publishes the schema-2 state and switches the
// targets through the engine. For an unfinished switch it only completes
// the switch. A failure after the publication leaves v2_restore
// "switching": a later run completes the switch, or restore-v2 gives
// release v2 back.
func (p *Proposal) Run(ctx context.Context) (engine.Result, error) {
	result := engine.Result{Status: "failed", Plan: p.Plan}
	if !p.Switching {
		if err := p.publish(ctx); err != nil {
			return result, err
		}
		if err := hit("published"); err != nil {
			return result, err
		}
	}
	return p.complete(ctx)
}
