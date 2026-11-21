package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/builds"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/hooks"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/targets"
	"github.com/luisgui1757/dotfiles/installer/terminal"
	"github.com/luisgui1757/dotfiles/installer/v2import"
)

func init() { importV2, restoreV2 = runImportV2, runRestoreV2 }

func legacyCommandDirectories(s setup) []string {
	if s.host.OS != "darwin" {
		return nil
	}
	st, err := state.Peek(s.stateDir, s.target)
	if err != nil {
		return nil
	}
	return v2import.LegacyCommandDirectories(s.stateDir, st, s.host.Context)
}

func verifyNvim(s setup) func(context.Context) error {
	return func(ctx context.Context) error {
		pins, err := builds.HostPins(s.catalog.Pins, s.host)
		if err != nil {
			return err
		}
		nvim := builds.CommandPath(s.stateDir, "tool.nvim", "nvim", pins["tool.nvim"])
		if nvim == "" {
			return errors.New("the Neovim build has no command path on this system")
		}
		_, err = platform.Exec{}.Run(ctx, platform.Command{Path: nvim, Args: []string{"--headless", "+if v:errmsg != '' | cquit 1 | endif", "+qa"}})
		return err
	}
}

func importOptions(s setup) (v2import.Options, func([]string), error) {
	if s.catalog == nil {
		return v2import.Options{}, nil, errors.New("the import from release v2 and restore-v2 require the catalog")
	}
	w := newWiring(s)
	if w.err != nil {
		return v2import.Options{}, nil, fmt.Errorf("the import from release v2 and restore-v2 require the provider wiring: %w", w.err)
	}
	e := &engine.Engine{Catalog: s.catalog, Router: w.route, Context: s.host.Context, StateDir: s.stateDir, Target: s.target, Source: s.source,
		Sweep: w.sweep, Selection: w.selected.set, Leftovers: w.leftovers}
	if s.sudo != "" {
		e.Elevate = sudoSession(s.sudo, s.authenticate)
	}
	kinds := map[string]targets.Kind{}
	maps.Copy(kinds, hooks.ProfileKinds())
	maps.Copy(kinds, hooks.SentinelKinds())
	maps.Copy(kinds, w.fonts.Kinds())
	options := v2import.Options{Engine: e, Folders: s.folders, Kinds: kinds, VerifyNvim: verifyNvim(s)}
	return options, func(directories []string) { w.profiles.LegacyCommandDirectories = directories }, nil
}

func importPreview(options v2import.Options, proposal *v2import.Proposal) string {
	return strings.Join(terminal.PlanText(options.Engine.Catalog, proposal.Plan), "\n") + "\n\n" + proposal.Details + "\n"
}

func reportImport(ui importUI, result engine.Result, err error) error {
	if err != nil && !errors.Is(err, engine.ErrReplan) && !errors.Is(err, engine.ErrPending) {
		result.Status, result.Message = "failed", strings.TrimSpace(err.Error()+"\n"+result.Message)
	}
	if reportErr := ui.Report(result); reportErr != nil {
		return errors.Join(err, reportErr)
	}
	if err != nil {
		err = &engine.ReportedError{Err: err}
	}
	return finish(result, err)
}

func runImportV2(ctx context.Context, s setup) error {
	options, setLegacy, err := importOptions(s)
	if err != nil {
		return err
	}
	c := s.command
	if c.name == "" {
		return importMenu(ctx, s, options, setLegacy)
	}
	if c.selectSet || len(c.removeShared) != 0 || len(c.replace) != 0 {
		return errors.New("the import from release v2 keeps the selection of release v2; run apply --yes without other arguments first, then change the selection. Nothing was changed")
	}
	proposal, err := v2import.Preview(ctx, options, setLegacy)
	if err != nil {
		return err
	}
	preview := importPreview(options, proposal)
	if c.name == "check" {
		if _, err := fmt.Fprint(s.output, preview); err != nil {
			return err
		}
		if proposal.Switching {
			return errors.New("the import from release v2 is not complete; run apply --yes to complete it, or restore-v2 --yes to go back to release v2")
		}
		return errors.New("this installation runs release v2; run setup, or apply --yes, to import it. Nothing was changed")
	}
	if !c.yes || c.name == "plan" {
		if c.name != "plan" {
			preview += "Nothing was changed. To import this installation, run the same command with --yes.\n"
		}
		_, err := fmt.Fprint(s.output, preview)
		return err
	}
	if _, err := fmt.Fprint(s.output, preview); err != nil {
		return err
	}
	ui := &terminal.Workflow{Output: s.output}
	applying, err := ui.BeginApply(ctx)
	if err != nil {
		return err
	}
	result, err := proposal.Run(applying)
	return reportImport(ui, result, err)
}

func importMenu(ctx context.Context, s setup, options v2import.Options, setLegacy func([]string)) (resultErr error) {
	ui := &terminal.Workflow{Input: os.Stdin, Output: s.output}
	defer func() { resultErr = errors.Join(resultErr, ui.Close()) }()
	open, err := reviewImport(ctx, ui, s, options, setLegacy)
	if err != nil || !open {
		return err
	}
	if err := ui.Close(); err != nil {
		return err
	}
	return menu(ctx, newEngine(s), os.Stdin, s.output)
}

type importUI interface {
	restoreUI
	BeginApply(ctx context.Context) (context.Context, error)
	Report(result engine.Result) error
}

func confirmPending(ctx context.Context, ui restoreUI, marks []string) ([]string, error) {
	options := []engine.Choice{
		{ID: "confirm", Label: "Yes, the command exited", Detail: "Setup clears the mark when the run that you approve starts. It inspects the system again and repeats the step only if it is still necessary."},
		{ID: "back", Label: "No, go back", Detail: "Wait until the command exits, then open setup again. Nothing changes now."},
	}
	for _, mark := range marks {
		choice, err := ui.Choose(ctx, "An earlier step did not report its end: "+mark+". Did its command exit?", options, []string{"back"}, false)
		if err != nil {
			return nil, err
		}
		if len(choice) != 1 || choice[0] != "confirm" {
			return nil, engine.ErrBack
		}
	}
	return slices.Clone(marks), nil
}

func reviewImport(ctx context.Context, ui importUI, s setup, options v2import.Options, setLegacy func([]string)) (bool, error) {
	notice := "This installation runs release v2. Review the import to release v3: setup builds the private tools first, and release v2 stays active when a build fails."
	for {
		proposal, err := v2import.Preview(ctx, options, setLegacy)
		if err != nil {
			return false, err
		}
		if proposal.Switching {
			choices := []engine.Choice{
				{ID: "complete", Label: "Complete the import", Detail: "Review and apply the remaining switch to release v3."},
				{ID: "restore", Label: "Restore release v2", Detail: "Give back the saved targets and the state of release v2."},
				{ID: "exit", Label: "Exit"},
			}
			choice, err := ui.Choose(ctx, "The import from release v2 is not complete. What would you like to do?", choices, []string{"complete"}, false)
			if errors.Is(err, engine.ErrCancelled) || err == nil && (len(choice) != 1 || choice[0] == "exit") {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			if choice[0] == "restore" {
				err := restoreMenu(ctx, ui, s, options)
				if errors.Is(err, engine.ErrBack) {
					continue
				}
				if errors.Is(err, engine.ErrCancelled) {
					return false, nil
				}
				return false, err
			}
			notice = "The import from release v2 is not complete. Review the remaining switch."
		}
		confirmed, err := confirmPending(ctx, ui, proposal.Plan.Pending)
		if errors.Is(err, engine.ErrBack) {
			continue
		}
		if errors.Is(err, engine.ErrCancelled) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		proposal.Confirmed = confirmed
		accepted, err := ui.Review(ctx, options.Engine.Catalog, engine.Proposal{Plan: proposal.Plan, Details: proposal.Details, Notice: notice}, nil)
		if errors.Is(err, engine.ErrCancelled) || err == nil && !accepted {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		applying, err := ui.BeginApply(ctx)
		if err != nil {
			return false, err
		}
		result, err := proposal.Run(applying)
		if errors.Is(err, engine.ErrPlanChanged) || errors.Is(err, engine.ErrReplan) {
			notice = "Your system changed after the review. This is the updated plan."
			if errors.Is(err, engine.ErrReplan) {
				notice = result.Message
			}
			continue
		}
		if errors.Is(err, engine.ErrPending) {
			continue
		}
		if err := reportImport(ui, result, err); err != nil || result.Plan.V2Restore != "open" {
			return false, err
		}
		return true, nil
	}
}

const restoreReplaceGroup = "Replace your changed content with the value of release v2 (setup saves your content first)"

type restoreUI interface {
	Choose(ctx context.Context, title string, choices []engine.Choice, initial []string, multiple bool) ([]string, error)
	Review(ctx context.Context, c engine.Catalog, proposal engine.Proposal, revise engine.Revise) (bool, error)
	Close() error
}

func restoreProposal(restoration *v2import.Restoration, replace []string) engine.Proposal {
	plan := engine.Plan{Mode: "restore-v2"}
	for _, item := range restoration.Items {
		action := map[string]string{"none": "keep", "restore": "repair", "remove": "remove"}[item.Action]
		switch {
		case item.Action == "changed" && slices.Contains(replace, item.Key):
			action = map[string]string{"restore": "repair", "remove": "remove"}[item.Replacement]
		case action == "":
			action = "pending"
		}
		plan.Operations = append(plan.Operations, engine.Operation{Resource: item.Resource, Action: action, Reason: item.Key + ": " + item.Detail})
	}
	proposal := engine.Proposal{Plan: plan, Details: restoration.Text(),
		Notice: fmt.Sprintf("Restore release v2: %d saved targets. Press d for the full list. Apply gives the targets back and then publishes the state of release v2.", len(restoration.Items))}
	for _, item := range restoration.Changed() {
		proposal.Choices = append(proposal.Choices, engine.ReviewChoice{Group: restoreReplaceGroup, ID: item.Key, Label: item.Key, Checked: slices.Contains(replace, item.Key),
			Detail: "Checked: setup saves your changed content, then writes the value of release v2. Unchecked: your content stays."})
	}
	return proposal
}

func restoreMenu(ctx context.Context, ui restoreUI, s setup, options v2import.Options) error {
	restoration, err := v2import.PreviewRestore(ctx, options)
	if err != nil {
		return err
	}
	if restoration.Confirmed, err = confirmPending(ctx, ui, restoration.Pending); err != nil {
		return err
	}
	replace := []string{}
	revise := func(_ context.Context, choices []engine.ReviewChoice) (engine.Proposal, error) {
		replace = []string{}
		for _, choice := range choices {
			if !slices.ContainsFunc(restoration.Changed(), func(item v2import.RestoreItem) bool { return item.Key == choice.ID }) {
				return engine.Proposal{}, fmt.Errorf("invalid restore choice %q", choice.ID)
			}
			if choice.Checked {
				replace = append(replace, choice.ID)
			}
		}
		return restoreProposal(restoration, replace), nil
	}
	accepted, err := ui.Review(ctx, options.Engine.Catalog, restoreProposal(restoration, replace), revise)
	if err != nil {
		return err
	}
	if !accepted {
		return engine.ErrBack
	}
	if err := ui.Close(); err != nil {
		return err
	}
	message, err := restoration.Run(ctx, replace)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(s.output, message)
	return err
}

func runRestoreV2(ctx context.Context, s setup) error {
	options, _, err := importOptions(s)
	if err != nil {
		return err
	}
	restoration, err := v2import.PreviewRestore(ctx, options)
	if err != nil {
		return err
	}
	preview := restoration.Text()
	if !s.command.yes && !restoration.Done {
		preview += "Nothing was changed. To restore release v2, run restore-v2 --yes. It keeps each target that you changed after the switch.\n"
	}
	if _, err := fmt.Fprint(s.output, preview); err != nil || !s.command.yes || restoration.Done {
		return err
	}
	message, err := restoration.Run(ctx, nil)
	if errors.Is(err, engine.ErrPending) {
		return reportImport(&terminal.Workflow{Output: s.output}, engine.Result{Status: "needs-action", Message: message}, err)
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(s.output, message)
	return err
}
