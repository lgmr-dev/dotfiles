package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lgmr-dev/dotfiles/installer/state"
)

const inspectionReuse = 5 * time.Minute

type inspection struct {
	found Findings
	at    time.Time
}

type backgroundInspection struct {
	key    string
	cancel context.CancelFunc
	done   chan struct{}
	found  Findings
	err    error
	at     time.Time
}

type menuSession struct {
	e           *Engine
	state       state.State
	drafts      map[string][]string
	inspected   map[string]inspection
	background  *backgroundInspection
	statusErr   error
	status      *ToolStatus
	checkFailed bool
}

func (s *menuSession) inspectionKey(request Request) (string, error) {
	wanted, released, err := Scope(s.e.Catalog, s.e.Context, s.state, request)
	if err != nil {
		return "", err
	}
	update := "pins"
	if request.Mode == "update" {
		update = "update"
	}
	return update + "\n" + strings.Join(sortedUnique(wanted), "\n") + "\n\n" + strings.Join(sortedUnique(released), "\n"), nil
}

func (s *menuSession) inspectInBackground(ctx context.Context, request Request) {
	key, err := s.inspectionKey(request)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	b := &backgroundInspection{key: key, cancel: cancel, done: make(chan struct{}), at: time.Now()}
	st := s.state
	go func() {
		defer close(b.done)
		b.found, b.err = s.e.inspect(ctx, st, request, viewRecords{st})
	}()
	s.background = b
}

func (s *menuSession) collect() {
	b := s.background
	if b == nil {
		return
	}
	select {
	case <-b.done:
	default:
		return
	}
	s.background = nil
	if b.err != nil {
		s.statusErr = b.err
		return
	}
	s.inspected[b.key] = inspection{found: b.found, at: b.at}
}

func (s *menuSession) close() {
	if b := s.background; b != nil {
		b.cancel()
		<-b.done
		s.background = nil
	}
}

func (s *menuSession) refresh() error {
	s.close()
	st, err := state.Peek(s.e.StateDir, s.e.Target)
	if err != nil {
		return err
	}
	s.state, s.inspected, s.statusErr, s.status = st, map[string]inspection{}, nil, nil
	return nil
}

func (s *menuSession) observe(ctx context.Context, request Request) (Findings, error) {
	key, err := s.inspectionKey(request)
	if err != nil {
		return Findings{}, err
	}
	if entry, ok := s.inspected[key]; ok && time.Since(entry.at) < inspectionReuse {
		return entry.found, nil
	}
	if b := s.background; b != nil {
		if b.key != key {
			b.cancel()
		}
		<-b.done
		s.background = nil
		if b.key == key && b.err == nil && time.Since(b.at) < inspectionReuse {
			s.inspected[key] = inspection{found: b.found, at: b.at}
			return b.found, nil
		}
	}
	at := time.Now()
	found, err := s.e.inspect(ctx, s.state, request, viewRecords{s.state})
	if err != nil {
		return Findings{}, err
	}
	s.inspected[key] = inspection{found: found, at: at}
	return found, nil
}

func (s *menuSession) plan(ctx context.Context, request Request) (Plan, error) {
	found, err := s.observe(ctx, request)
	if err != nil {
		return Plan{}, err
	}
	return PlanChanges(s.e.Catalog, s.e.Context, s.state, request, found, s.e.Source)
}

func (s *menuSession) toolStatus() ToolStatus {
	if s.status != nil {
		return *s.status
	}
	status := s.computeToolStatus()
	if status.Ready {
		s.status = &status
	}
	return status
}

func (s *menuSession) computeToolStatus() ToolStatus {
	if len(s.state.Selected) == 0 {
		return ToolStatus{Ready: true}
	}
	s.collect()
	request := Request{Mode: "apply", Selected: s.state.Selected, SelectSet: true}
	key, err := s.inspectionKey(request)
	if err != nil {
		return ToolStatus{Ready: true, Summary: "Could not check installed tools: " + err.Error()}
	}
	entry, ok := s.inspected[key]
	switch {
	case ok:
	case s.background != nil && s.background.key == key:
		return ToolStatus{Summary: "Checking installed tools..."}
	case s.statusErr != nil:
		return ToolStatus{Ready: true, Summary: "Could not check installed tools: " + s.statusErr.Error()}
	default:
		return ToolStatus{Ready: true}
	}
	current, err := PlanChanges(s.e.Catalog, s.e.Context, s.state, request, entry.found, s.e.Source)
	if err != nil {
		return ToolStatus{Ready: true, Summary: "Could not check installed tools: " + err.Error()}
	}
	tools := map[string]string{}
	rank := map[string]int{"update available": 1, "needs repair": 2, "needs action": 3}
	mark := func(op Operation, label string) {
		for _, root := range op.RequiredBy {
			if rank[label] > rank[tools[root]] {
				tools[root] = label
			}
		}
	}
	for _, op := range current.Operations {
		switch {
		case op.Action == "pending":
			mark(op, "needs action")
		case op.Action == "repair":
			mark(op, "needs repair")
		case op.Action == "install":
			if r, _ := s.e.Catalog.Resource(op.Resource); Recorded(s.state, r, s.e.Context) {
				mark(op, "needs repair")
			} else {
				mark(op, "update available")
			}
		case op.Action == "keep" && op.Finding.Status == Outdated:
			mark(op, "update available")
		}
	}
	summary := fmt.Sprintf("All %d installed tools are healthy and current.", len(s.state.Selected))
	if len(tools) > 0 {
		summary = fmt.Sprintf("%d of %d installed tools need attention.", len(tools), len(s.state.Selected))
	}
	return ToolStatus{Ready: true, Summary: summary, Tools: tools}
}

func reviewChoiceKey(group, id string) string { return group + "\x00" + id }

func (s *menuSession) propose(ctx context.Context, base Request, keepChoices bool, checked map[string]bool) (Proposal, Request, error) {
	request := base
	choices := []ReviewChoice{}
	isChecked := func(group, id string, initial bool) bool {
		if value, ok := checked[reviewChoiceKey(group, id)]; ok {
			return value
		}
		return initial
	}
	if keepChoices && len(s.state.Keep) != 0 {
		request.Keep = []string{}
		for _, id := range s.state.Keep {
			r, ok := s.e.Catalog.Resource(id)
			if !ok {
				return Proposal{}, request, fmt.Errorf("retained resource %s requires catalog migration", id)
			}
			keep := isChecked(ReviewKeep, id, true)
			choices = append(choices, ReviewChoice{Group: ReviewKeep, ID: id, Label: r.Name, Detail: "Keep this resource even without a selected tool.", Checked: keep})
			if keep {
				request.Keep = append(request.Keep, id)
			}
		}
	}
	plan, err := s.plan(ctx, request)
	if err != nil {
		return Proposal{}, request, err
	}
	replacements := []ReviewChoice{}
	for _, op := range plan.Operations {
		if op.Finding.Status == Changed && op.Action == "pending" {
			r, _ := s.e.Catalog.Resource(op.Resource)
			replacements = append(replacements, ReviewChoice{Group: ReviewReplace, ID: r.ID, Label: r.Name, Detail: "Save the current content separately, then apply the selected content. Unchecked, your changed content stays as it is."})
		}
	}
	if len(replacements) != 0 {
		request.Replace = []string{}
		for i := range replacements {
			replacements[i].Checked = isChecked(ReviewReplace, replacements[i].ID, false)
			if replacements[i].Checked {
				request.Replace = append(request.Replace, replacements[i].ID)
			}
		}
		choices = append(choices, replacements...)
		if plan, err = s.plan(ctx, request); err != nil {
			return Proposal{}, request, err
		}
	}
	if request.Mode == "apply" {
		shared, err := s.sharedRemovalChoices(ctx, request, plan)
		if err != nil {
			return Proposal{}, request, err
		}
		if len(shared) != 0 {
			request.RemoveShared = []string{}
			for i := range shared {
				shared[i].Checked = isChecked(ReviewShared, shared[i].ID, false)
				if shared[i].Checked {
					request.RemoveShared = append(request.RemoveShared, shared[i].ID)
				}
			}
			choices = append(choices, shared...)
			if plan, err = s.plan(ctx, request); err != nil {
				return Proposal{}, request, err
			}
		}
	}
	return Proposal{Plan: plan, Choices: choices, Notice: FallbackNotice(plan)}, request, nil
}

func (s *menuSession) sharedRemovalChoices(ctx context.Context, request Request, plan Plan) ([]ReviewChoice, error) {
	choices := []ReviewChoice{}
	trial := request
	trial.RemoveShared = slices.Clone(request.RemoveShared)
	for _, op := range plan.Operations {
		if op.Action != "retain" || op.Reason != reasonShared {
			continue
		}
		r, _ := s.e.Catalog.Resource(op.Resource)
		choices = append(choices, ReviewChoice{Group: ReviewShared, ID: r.ID, Label: r.Name, Detail: "Created by this installer; another application may use it. " + op.Reason})
		trial.RemoveShared = append(trial.RemoveShared, r.ID)
	}
	if len(choices) == 0 {
		return choices, nil
	}
	removal, err := s.plan(ctx, trial)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(choices, func(choice ReviewChoice) bool {
		return !slices.ContainsFunc(removal.Operations, func(op Operation) bool { return op.Resource == choice.ID && op.Action == "remove" })
	}), nil
}

func recordChoices(offered, returned []ReviewChoice, checked map[string]bool) error {
	if len(offered) != len(returned) {
		return errors.New("review returned a different set of choices")
	}
	seen := map[string]bool{}
	for _, choice := range returned {
		key := reviewChoiceKey(choice.Group, choice.ID)
		if seen[key] || !slices.ContainsFunc(offered, func(o ReviewChoice) bool { return o.Group == choice.Group && o.ID == choice.ID }) {
			return fmt.Errorf("invalid review choice %q", choice.ID)
		}
		seen[key] = true
		checked[key] = choice.Checked
	}
	return nil
}

func validateChoice(selected []string, options []Choice, multiple bool) error {
	if !multiple && len(selected) != 1 {
		return errors.New("menu requires exactly one choice")
	}
	seen := map[string]bool{}
	for _, id := range selected {
		if seen[id] || !slices.ContainsFunc(options, func(o Choice) bool { return o.ID == id }) {
			return fmt.Errorf("invalid menu choice %q", id)
		}
		seen[id] = true
	}
	return nil
}

// Run is the interactive flow: the action menu, the tool menu, the review,
// the approved run and its report. A first run opens the tool menu directly.
// Going back from a later screen returns to the action menu and keeps the
// tool choices. A new plan is never accepted for the operator: a changed
// system or a new provider brings back the review.
func (e *Engine) Run(ctx context.Context) (result Result, resultErr error) {
	result.Status = "cancelled"
	defer func() {
		if errors.Is(resultErr, ErrCancelled) {
			resultErr = nil
			result = Result{Status: "cancelled"}
		}
		if resultErr != nil {
			result.Status = "failed"
		}
	}()
	if err := e.validate(); err != nil {
		return result, err
	}
	ui := e.UI
	if ui == nil {
		return result, errors.New("interactive controller requires an input/output surface")
	}
	st, err := state.Peek(e.StateDir, e.Target)
	if err != nil {
		return result, err
	}
	actions := []Choice{
		{ID: "apply", Label: "Choose tools", Detail: "Install, remove or repair tools; required dependencies are included automatically."},
		{ID: "update", Label: "Update tools", Detail: "Apply this checkout's reviewed versions and the latest Sentinel release, and repair failed checks; your selection stays the same."},
		{ID: "check", Label: "Check installation", Detail: "Inspect health and required actions without changing anything."},
	}
	if e.RestoreV2 != nil && st.V2Restore == "open" {
		actions = append(actions, Choice{ID: "restore-v2", Label: "Restore release v2", Detail: "Give back the files and state of release v2; changed files stay unless you choose to replace them"})
	}
	actions = append(actions, Choice{ID: "exit", Label: "Exit"})
	session := &menuSession{e: e, state: st, drafts: map[string][]string{}, inspected: map[string]inspection{}}
	defer session.close()
	if st.Schema != 0 && len(st.Selected)+len(st.Keep) != 0 {
		session.inspectInBackground(ctx, Request{Mode: "apply", Selected: st.Selected, SelectSet: true})
	}
	menu := st.Schema != 0
	for {
		mode := "apply"
		if menu {
			initial := []string{"apply"}
			if session.state.Source != "" && session.state.Source != e.Source || session.checkFailed {
				initial = []string{"update"}
			}
			choice, err := ui.Choose(ctx, "What would you like to do?", actions, initial, false)
			if errors.Is(err, ErrBack) {
				continue
			}
			if err != nil {
				return result, err
			}
			if err := validateChoice(choice, actions, false); err != nil {
				return result, err
			}
			mode = choice[0]
		}
		menu = true
		if mode == "exit" {
			return result, nil
		}
		if mode == "restore-v2" {
			session.close()
			result, resultErr = e.RestoreV2(ctx, ui)
			if !errors.Is(resultErr, ErrBack) {
				return result, resultErr
			}
			result = Result{Status: "cancelled"}
			continue
		}
		result, resultErr = e.runMode(ctx, ui, session, mode)
		if !errors.Is(resultErr, ErrBack) && (mode != "check" || resultErr != nil) {
			return result, resultErr
		}
		result = Result{Status: "cancelled"}
	}
}

func confirmPending(ctx context.Context, ui Interaction, marks []string) ([]string, error) {
	options := []Choice{
		{ID: "confirm", Label: "Yes, the command exited", Detail: "Setup clears the mark when the run that you approve starts. It inspects the system again and repeats the step only if it is still necessary."},
		{ID: "back", Label: "No, go back", Detail: "Wait until the command exits, then open setup again. Nothing changes now."},
	}
	for _, mark := range marks {
		choice, err := ui.Choose(ctx, "An earlier step did not report its end: "+mark+". Did its command exit?", options, []string{"back"}, false)
		if err != nil {
			return nil, err
		}
		if err := validateChoice(choice, options, false); err != nil {
			return nil, err
		}
		if choice[0] != "confirm" {
			return nil, ErrBack
		}
	}
	return slices.Clone(marks), nil
}

func (e *Engine) runMode(ctx context.Context, ui Interaction, session *menuSession, mode string) (Result, error) {
	result := Result{Status: "cancelled"}
	request := Request{Mode: mode}
	if mode == "check" {
		plan, err := session.plan(ctx, request)
		if err != nil {
			return result, err
		}
		result = e.checkResult(ctx, session.state, plan)
		session.checkFailed = result.Status != "ready"
		return result, ui.Report(result)
	}
	confirmed, err := confirmPending(ctx, ui, session.state.Pending)
	if err != nil {
		return result, err
	}
	if mode == "apply" {
		options := []Choice{}
		for _, r := range e.Catalog.Menu(e.Context) {
			options = append(options, Choice{ID: r.ID, Label: r.Name, Detail: r.Description, Category: r.Category})
		}
		initial := session.state.Selected
		if draft, ok := session.drafts[mode]; ok {
			initial = draft
		}
		selected := []string{}
		if len(options) > 0 {
			var err error
			selected, err = ui.ChooseTools(ctx, ToolMenu{Title: "Choose the tools you want installed", Choices: options, Initial: initial, Installed: session.state.Selected, Status: session.toolStatus})
			if err != nil {
				return result, err
			}
			if err := validateChoice(selected, options, true); err != nil {
				return result, err
			}
			session.drafts[mode] = selected
		}
		request.Selected, request.SelectSet = selected, true
	}
	checked := map[string]bool{}
	notice := ""
	for {
		proposal, current, err := session.propose(ctx, request, mode == "apply", checked)
		if err != nil {
			return result, err
		}
		if notice != "" {
			proposal.Notice = strings.TrimSpace(notice + " " + proposal.Notice)
		}
		revise := func(ctx context.Context, choices []ReviewChoice) (Proposal, error) {
			if err := recordChoices(proposal.Choices, choices, checked); err != nil {
				return Proposal{}, err
			}
			next, nextRequest, err := session.propose(ctx, request, mode == "apply", checked)
			if err != nil {
				return Proposal{}, err
			}
			proposal, current = next, nextRequest
			return proposal, nil
		}
		accepted, err := ui.Review(ctx, e.Catalog, proposal, revise)
		if err != nil || !accepted {
			return Result{Status: "cancelled"}, err
		}
		session.close()
		applying, err := ui.BeginApply(ctx)
		if err != nil {
			return result, err
		}
		result, err = e.apply(applying, current, proposal.Plan, confirmed)
		if errors.Is(err, ErrPlanChanged) || errors.Is(err, ErrReplan) {
			if refreshErr := session.refresh(); refreshErr != nil {
				return result, refreshErr
			}
			notice = "Your system changed after the review. This is the updated plan."
			if errors.Is(err, ErrReplan) {
				notice = result.Message
			}
			continue
		}
		if errors.Is(err, ErrPending) {
			if refreshErr := session.refresh(); refreshErr != nil {
				return result, refreshErr
			}
			if confirmed, err = confirmPending(ctx, ui, session.state.Pending); err != nil {
				return result, err
			}
			notice = ""
			continue
		}
		if err != nil {
			result.Status, result.Message = "failed", strings.TrimSpace(err.Error()+"\n"+result.Message)
		}
		if reportErr := ui.Report(result); reportErr != nil || err == nil {
			return result, errors.Join(err, reportErr)
		}
		return result, &ReportedError{Err: err}
	}
}
