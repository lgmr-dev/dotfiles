package engine

import (
	"fmt"
	"slices"
	"sort"

	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
)

// Findings holds the inspections that a plan needs. Wanted has the resources
// of the closure, inspected with Want.Present. Released has the recorded
// resources outside the closure, inspected without it.
type Findings struct {
	Wanted   map[string]Finding
	Released map[string]Finding
}

const (
	reasonShared     = "external use cannot be excluded; explicit per-item removal required"
	reasonSystem     = "setup never removes a system package or vendor application; its dependencies stay"
	reasonRetainMark = "retained infrastructure or personal data"
	reasonLaterApply = "selection changes and removal require a separate apply request"
	reasonRetainedBy = "required by another retained resource"
)

func sortedUnique(values []string) []string {
	result := append([]string{}, values...)
	sort.Strings(result)
	return slices.Compact(result)
}

func mutating(action string) bool {
	return action == "install" || action == "repair" || action == "update" || action == "remove" || action == "forget"
}

func resolve(c Catalog, st state.State, request Request) (Request, error) {
	switch request.Mode {
	case "apply":
		if !request.SelectSet {
			request.Selected = st.Selected
		}
		if request.Keep == nil {
			request.Keep = st.Keep
		}
	case "update", "check":
		if request.SelectSet || request.Keep != nil || len(request.RemoveShared) != 0 || request.Mode == "check" && len(request.Replace) != 0 {
			return request, fmt.Errorf("%s preserves the recorded selection and Keep choices; use apply to change them", request.Mode)
		}
		request.Selected, request.Keep = st.Selected, st.Keep
	default:
		return request, fmt.Errorf("unknown mode %q", request.Mode)
	}
	request.Selected, request.Keep, request.RemoveShared = sortedUnique(request.Selected), sortedUnique(request.Keep), sortedUnique(request.RemoveShared)
	request.Replace = sortedUnique(request.Replace)
	for _, id := range request.Selected {
		if r, ok := c.Resource(id); !ok || !r.Capability {
			return request, fmt.Errorf("unknown capability %q", id)
		}
	}
	for _, id := range request.Keep {
		if _, ok := c.Resource(id); !ok {
			return request, fmt.Errorf("unknown retained resource %q", id)
		}
	}
	for _, id := range request.RemoveShared {
		if slices.Contains(request.Keep, id) {
			return request, fmt.Errorf("%s cannot be both kept and removed", id)
		}
	}
	return request, nil
}

// Scope returns the resources that a request needs inspected. Wanted is the
// closure of the selected and kept resources in dependency order. Released
// has the resources with a record outside that closure, consumers before
// their requirements.
func Scope(c Catalog, ctx platform.Context, st state.State, request Request) (wanted, released []string, err error) {
	if err := ctx.Validate(); err != nil {
		return nil, nil, err
	}
	request, err = resolve(c, st, request)
	if err != nil {
		return nil, nil, err
	}
	wanted, err = c.Closure(slices.Concat(request.Selected, request.Keep), ctx)
	if err != nil {
		return nil, nil, err
	}
	menu := []string{}
	for _, r := range c.Menu(ctx) {
		menu = append(menu, r.ID)
	}
	all, err := c.Closure(menu, ctx)
	if err != nil {
		return nil, nil, err
	}
	recorded := []string{}
	for _, id := range all {
		if r, _ := c.Resource(id); SystemRecorded(st, r, ctx) {
			recorded = append(recorded, id)
		}
	}
	for _, record := range st.Targets {
		recorded = append(recorded, record.Resource)
	}
	recorded = slices.DeleteFunc(sortedUnique(recorded), func(id string) bool { return slices.Contains(wanted, id) })
	order, err := c.Closure(recorded, ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("the state records a resource that this checkout cannot plan; update the checkout or use the release that wrote the state: %w", err)
	}
	slices.Reverse(order)
	for _, id := range order {
		if r, _ := c.Resource(id); r.Action != "" && slices.Contains(recorded, id) {
			released = append(released, id)
		}
	}
	return wanted, released, nil
}

func withReason(text, reason string) string {
	if reason == "" {
		return text
	}
	return text + ": " + reason
}

// PlanChanges derives the plan from the state, the request and the
// inspections. It is pure: inspections, prompts and changes are outside it.
func PlanChanges(c Catalog, ctx platform.Context, st state.State, request Request, found Findings, source string) (Plan, error) {
	wanted, released, err := Scope(c, ctx, st, request)
	if err != nil {
		return Plan{}, err
	}
	request, err = resolve(c, st, request)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{Mode: request.Mode, Source: source, Target: st.Target, Context: ctx, Selected: request.Selected, Keep: request.Keep,
		RemoveShared: request.RemoveShared, Replace: request.Replace, Pending: slices.Clone(st.Pending)}
	for _, id := range request.Replace {
		if f, ok := found.Wanted[id]; !ok || !slices.Contains(wanted, id) || f.Status != Changed {
			return plan, fmt.Errorf("%s has no changed content to replace in this selection", id)
		}
	}
	for _, id := range request.RemoveShared {
		if r, ok := c.Resource(id); !ok || !Recorded(st, r, ctx) {
			return plan, fmt.Errorf("cannot remove unrecorded resource %q", id)
		}
	}
	requiredBy := map[string][]string{}
	for _, root := range plan.Selected {
		closure, err := c.Closure([]string{root}, ctx)
		if err != nil {
			return plan, err
		}
		for _, id := range closure {
			requiredBy[id] = append(requiredBy[id], root)
		}
	}
	blocked := map[string]string{}
	for _, id := range wanted {
		r, _ := c.Resource(id)
		if r.Action == "" {
			continue
		}
		f, ok := found.Wanted[id]
		if !ok {
			return plan, fmt.Errorf("missing inspection for %s", id)
		}
		op := Operation{Resource: id, Action: "keep", Reason: "already satisfies selection", RequiredBy: requiredBy[id], Privileged: f.Privileged, Commands: f.Commands, Finding: f}
		blocks := false
		switch f.Status {
		case Satisfied:
		case Missing, Absent:
			op.Action, op.Reason = "install", withReason("required by selection", f.Reason)
		case Unhealthy:
			op.Action, op.Reason = "repair", withReason("selected resource failed its check", f.Reason)
		case Outdated:
			if request.Mode == "update" {
				op.Action, op.Reason = "update", withReason("explicit update to the reviewed version", f.Reason)
			} else {
				op.Reason = withReason("a newer reviewed version is available; choose Update tools", f.Reason)
			}
		case Changed:
			op.Action, op.Reason = "pending", withReason("changed content stays as it is", f.Reason)
			if slices.Contains(request.Replace, id) {
				op.Action, op.Reason = "repair", withReason("save the current content, then replace it with the selected content", f.Reason)
				op.Finding.Replace = true
			}
		case Unknown:
			op.Action, op.Reason, blocks = "pending", withReason("setup could not inspect this resource", f.Reason), true
		case Blocked:
			op.Action, op.Reason, blocks = "pending", f.Reason, true
			if op.Reason == "" {
				op.Reason = "another action is necessary first"
			}
		default:
			return plan, fmt.Errorf("%s has an invalid inspection status", id)
		}
		if (r.ReplanAfter || f.Replan) && (op.Action == "install" || op.Action == "repair" || op.Action == "update") {
			op.Reason += "; bootstrap then review newly available provider inventory"
		}
		closure, err := c.Closure([]string{id}, ctx)
		if err != nil {
			return plan, err
		}
		for _, dependency := range closure {
			if reason, waiting := blocked[dependency]; waiting && dependency != id {
				op.Action, op.Reason, blocks = "pending", "waiting for "+dependency+": "+reason, true
				break
			}
		}
		if blocks {
			blocked[id] = op.Reason
		}
		plan.Operations = append(plan.Operations, op)
	}
	first := len(plan.Operations)
	retained := []string{}
	for _, id := range released {
		r, _ := c.Resource(id)
		f, ok := found.Released[id]
		if !ok {
			return plan, fmt.Errorf("missing removal inspection for %s", id)
		}
		op := Operation{Resource: id, Action: "remove", Reason: "no remaining selected consumer", Privileged: f.Privileged, Commands: f.Commands, Finding: f}
		switch {
		case request.Mode != "apply":
			op.Action, op.Reason = "retain", reasonLaterApply
		case f.Status == Unknown || f.Status == Blocked:
			op.Action, op.Reason = "retain", withReason("setup could not inspect this resource", f.Reason)
		case f.Status == Changed:
			op.Action, op.Reason = "retain", withReason("changed content stays as it is", f.Reason)
		case f.Status == Absent || f.Status == Missing:
			op.Action, op.Reason = "forget", "already absent"
		case r.Retain:
			op.Action, op.Reason = "retain", reasonRetainMark
		case SystemRecorded(st, r, ctx):
			op.Action, op.Reason = "retain", reasonSystem
		case r.Shared && !slices.Contains(request.RemoveShared, id):
			op.Action, op.Reason = "retain", reasonShared
		}
		if op.Action == "retain" {
			retained = append(retained, id)
		}
		plan.Operations = append(plan.Operations, op)
	}
	held := map[string]bool{}
	for _, id := range retained {
		closure, err := c.Closure([]string{id}, ctx)
		if err != nil {
			return plan, err
		}
		for _, dependency := range closure {
			if dependency != id {
				held[dependency] = true
			}
		}
	}
	for i := first; i < len(plan.Operations); i++ {
		op := &plan.Operations[i]
		if op.Action == "remove" && held[op.Resource] {
			op.Action, op.Reason = "retain", reasonRetainedBy
		}
		if op.Action != "retain" || op.Reason != reasonSystem || held[op.Resource] {
			op.Commands = slices.DeleteFunc(slices.Clone(op.Commands), func(block CommandBlock) bool { return block.By == "you" })
		}
	}
	return plan, nil
}
