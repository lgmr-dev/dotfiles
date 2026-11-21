package terminal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/engine"
	"golang.org/x/term"
)

// Workflow connects the native terminal to the engine. One full-screen
// session spans every menu and review; each interaction restores cooked input
// before returning. BeginApply releases the screen before any approved
// change, provider prompt or elevation, and the progress of that work stays
// in the ordinary scrollback. Close releases a remaining screen. A run
// without the menu uses only BeginApply and Report, and needs no Input.
type Workflow struct {
	Input, Output *os.File
	screen        *Console
	progress      *progressView
}

func (w *Workflow) open() (*Console, error) {
	if err := w.closeProgress(); err != nil {
		return nil, err
	}
	if w.screen == nil {
		c, err := Open(w.Input, w.Output)
		if err != nil {
			return nil, err
		}
		w.screen = c
	}
	return w.screen, nil
}

// Close releases the screen and ends an apply display whose result was never
// reported.
func (w *Workflow) Close() error {
	return errors.Join(w.closeScreen(), w.closeProgress())
}

func (w *Workflow) closeScreen() error {
	if w.screen == nil {
		return nil
	}
	err := w.screen.Close()
	w.screen = nil
	return err
}

func (w *Workflow) closeProgress() error {
	if w.progress == nil {
		return nil
	}
	err := w.progress.Close()
	w.progress = nil
	return err
}

func engineError(err error) error {
	switch {
	case errors.Is(err, ErrBack):
		return engine.ErrBack
	case errors.Is(err, ErrCancelled):
		return engine.ErrCancelled
	}
	return err
}

// Choose shows a menu and returns the chosen IDs.
func (w *Workflow) Choose(ctx context.Context, title string, choices []engine.Choice, initial []string, multiple bool) ([]string, error) {
	c, err := w.open()
	if err != nil {
		return nil, err
	}
	options := make([]Option, 0, len(choices))
	for _, choice := range choices {
		options = append(options, Option{ID: choice.ID, Label: choice.Label, Detail: choice.Detail, Category: choice.Category})
	}
	selected, err := c.Choose(ctx, title, options, initial, multiple)
	if err == nil {
		err = c.Busy("Inspecting your system...")
	}
	return selected, engineError(err)
}

// ChooseTools shows the tool selection screen with the status of the
// installed tools.
func (w *Workflow) ChooseTools(ctx context.Context, menu engine.ToolMenu) ([]string, error) {
	c, err := w.open()
	if err != nil {
		return nil, err
	}
	options := make([]Option, 0, len(menu.Choices))
	for _, choice := range menu.Choices {
		options = append(options, Option{ID: choice.ID, Label: choice.Label, Detail: choice.Detail, Category: choice.Category})
	}
	screen := ToolScreen{Installed: menu.Installed}
	if menu.Status != nil {
		screen.Status = func() (string, map[string]string) {
			status := menu.Status()
			return status.Summary, status.Tools
		}
	}
	selected, err := c.ChooseTools(ctx, menu.Title, options, menu.Initial, screen)
	if err == nil {
		err = c.Busy("Inspecting your system...")
	}
	return selected, engineError(err)
}

// Review shows the confirmation screen. It returns true only after an
// explicit approval.
func (w *Workflow) Review(ctx context.Context, catalog engine.Catalog, proposal engine.Proposal, revise engine.Revise) (bool, error) {
	c, err := w.open()
	if err != nil {
		return false, err
	}
	accepted, err := c.confirm(ctx, catalog, proposal, revise)
	return accepted, engineError(err)
}

// BeginApply releases the screen and starts the progress display. On a
// terminal the display rewrites lines with escape sequences, so Windows
// virtual terminal output stays enabled until the display ends. Redirected
// output gets plain lines.
func (w *Workflow) BeginApply(ctx context.Context) (context.Context, error) {
	if err := w.Close(); err != nil {
		return ctx, err
	}
	view := &progressView{output: w.Output, plain: true}
	if term.IsTerminal(int(w.Output.Fd())) {
		restore, err := prepareOutput(w.Output)
		if err != nil {
			return ctx, err
		}
		view.plain, view.restore = false, restore
	}
	w.progress = view
	return engine.WithProgress(ctx, engine.Progress{Step: w.progress.Step, Output: w.progress}), nil
}

// Report shows the result of a check on its own screen and each other result
// in the ordinary scrollback.
func (w *Workflow) Report(result engine.Result) error {
	if result.Plan.Mode == "check" {
		c, err := w.open()
		if err != nil {
			return err
		}
		lines := append([]string{reportHeadline(result.Status), ""}, strings.Split(result.Message, "\n")...)
		return engineError(c.Read(context.Background(), "Installation check", lines))
	}
	closeErr := w.Close()
	_, err := fmt.Fprint(w.Output, ReportText(result))
	return errors.Join(closeErr, err)
}

func reportHeadline(status string) string {
	switch status {
	case "ready":
		return "Done: everything selected is ready."
	case "needs-action":
		return "Finished, but these items need your action:"
	case "failed":
		return "Failed."
	case "cancelled":
		return "Cancelled; nothing was changed."
	}
	return status
}

const commandIndent = "    "

type reviewLine struct {
	text    string
	command bool
}

func textLines(texts []string) []reviewLine {
	lines := make([]reviewLine, 0, len(texts))
	for _, text := range texts {
		lines = append(lines, reviewLine{text: text})
	}
	return lines
}

func commandLines(blocks []engine.CommandBlock) []reviewLine {
	lines := []reviewLine{}
	for _, block := range blocks {
		if block.Title != "" {
			lines = append(lines, reviewLine{text: block.Title})
		}
		label := "Setup runs:"
		if block.By == "you" {
			label = "Run these commands yourself:"
		}
		lines = append(lines, reviewLine{text: label})
		for _, line := range block.Lines {
			lines = append(lines, reviewLine{text: commandIndent + strings.ReplaceAll(cleanOutput(line), "\t", " "), command: true})
		}
	}
	return lines
}

// ReportText is the final report of a run. Disclosures follow the actionable
// reasons under their own heading, so a note never reads as the failure. A
// failure is one message whose continuation lines, such as native output,
// stay together beneath it. The commands that the operator runs follow, each
// block under its title. A command line has only its indent, so a copy of the
// line runs as it is.
func ReportText(result engine.Result) string {
	var actions, notes []string
	for _, line := range strings.Split(result.Message, "\n") {
		line = plain(strings.TrimRight(line, " \t\r"))
		switch {
		case strings.TrimSpace(line) == "":
		case strings.HasPrefix(line, "Preserved files for inspection: "), strings.HasPrefix(line, "Note: "), strings.HasPrefix(line, "Apply log "):
			notes = append(notes, "  - "+line)
		case result.Status == "failed" && len(actions) > 0:
			actions = append(actions, "    "+line)
		default:
			actions = append(actions, "  - "+strings.TrimSpace(line))
		}
	}
	text := "\n==> " + reportHeadline(result.Status) + "\n"
	if len(actions) > 0 {
		text += strings.Join(actions, "\n") + "\n"
	}
	if len(notes) > 0 {
		text += "Notes (informational, not failures):\n" + strings.Join(notes, "\n") + "\n"
	}
	if result.Status != "cancelled" {
		for _, op := range result.Plan.Operations {
			for _, block := range op.Commands {
				if block.By != "you" {
					continue
				}
				text += "\n"
				for _, line := range commandLines([]engine.CommandBlock{block}) {
					text += plain(line.text) + "\n"
				}
			}
		}
	}
	if result.Log != "" {
		text += "Full log: " + plain(result.Log) + "\n"
	}
	return text
}

func changes(action string) bool {
	return action == "install" || action == "update" || action == "repair"
}

func removes(action string) bool { return action == "remove" || action == "forget" }

func planSummary(plan engine.Plan) []string {
	counts := map[string]int{}
	admin := 0
	for _, op := range plan.Operations {
		counts[op.Action]++
		if op.Privileged && (changes(op.Action) || removes(op.Action)) {
			admin++
		}
	}
	return []string{
		fmt.Sprintf("Add: %d  Change: %d", counts["install"], counts["repair"]+counts["update"]),
		fmt.Sprintf("Remove: %d  Keep: %d", counts["remove"]+counts["forget"], counts["keep"]+counts["retain"]),
		fmt.Sprintf("Needs action: %d  Admin: %d", counts["pending"], admin),
	}
}

func planHighlights(catalog engine.Catalog, plan engine.Plan) []string {
	lines := []string{}
	for _, op := range plan.Operations {
		r, _ := catalog.Resource(op.Resource)
		switch {
		case removes(op.Action) || op.Privileged && changes(op.Action):
			lines = append(lines, strings.ToUpper(op.Action)+": "+r.Name)
		case op.Action == "pending":
			lines = append(lines, "NEEDS ACTION: "+r.Name+" - "+op.Reason)
		}
	}
	return lines
}

func planText(catalog engine.Catalog, plan engine.Plan, details string) []reviewLine {
	lines := textLines(append(planSummary(plan), planHighlights(catalog, plan)...))
	add := func(texts ...string) { lines = append(lines, textLines(texts)...) }
	add("", "Operation: "+plan.Mode, "Target: "+plan.Target, "Source: "+plan.Source,
		"Selected: "+strings.Join(plan.Selected, ", "), "Keep: "+strings.Join(plan.Keep, ", "))
	for _, mark := range plan.Pending {
		add("Unfinished step: " + mark)
	}
	add("")
	if len(plan.Operations) == 0 {
		add("No resource changes.")
	}
	for _, op := range plan.Operations {
		r, _ := catalog.Resource(op.Resource)
		add(strings.ToUpper(op.Action)+": "+r.Name+" ("+r.ID+")", "Why: "+op.Reason)
		if len(op.RequiredBy) > 0 {
			add("Required by: " + strings.Join(op.RequiredBy, ", "))
		}
		if provider := r.Bindings[plan.Context.OS].Provider; provider != "" {
			add("Provider: " + provider)
		}
		if op.Finding.Version != "" {
			add("Installed version: " + op.Finding.Version)
		}
		for _, path := range engine.ExistingPreservedPaths(op.Finding.Preserved) {
			add("Preserved files for inspection: " + path)
		}
		for _, note := range op.Finding.Notes {
			add("Note: " + note)
		}
		if op.Privileged {
			add("Requires administrator privileges.")
		}
		lines = append(lines, commandLines(op.Commands)...)
		add("")
	}
	if details != "" {
		add("Exact reviewed scope:")
		add(strings.Split(details, "\n")...)
	}
	return lines
}

// PlanText is the preview of a plan as plain lines: the counts, the changes
// that need attention, then each operation with its reason and its command
// blocks. A command line has only its indent, so a copy of the line runs as
// it is.
func PlanText(catalog engine.Catalog, plan engine.Plan) []string {
	texts := []string{}
	for _, line := range planText(catalog, plan, "") {
		texts = append(texts, plain(line.text))
	}
	return texts
}

type screenRow struct {
	text   string
	joined bool
}

func layout(lines []reviewLine, width int) []screenRow {
	rows := []screenRow{}
	for _, line := range lines {
		if !line.command {
			for _, text := range wrapLines([]string{line.text}, width-1) {
				rows = append(rows, screenRow{text: text})
			}
			continue
		}
		rest := plain(line.text)
		for rest != "" {
			part := fit(rest, width)
			rest = rest[len(part):]
			rows = append(rows, screenRow{text: part, joined: rest != ""})
		}
	}
	return rows
}

// Read is a scrollable document, not a clipped option detail. Every approval
// field and explanation remains reachable in narrow or resized terminals.
// Enter and Esc both close it; Ctrl-C quits.
func (c *Console) Read(ctx context.Context, title string, lines []string) error {
	return c.interact(func() error { return c.read(ctx, title, textLines(lines)) })
}

func (c *Console) read(ctx context.Context, title string, lines []reviewLine) error {
	offset, previous := 0, ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		width, height, err := term.GetSize(int(c.output.Fd()))
		if err != nil {
			return err
		}
		if width < 32 || height < 10 {
			return errors.New("enlarge the terminal to at least 32 columns and 10 rows, then try again")
		}
		wrapped := layout(lines, width)
		rows := height - 5
		offset = min(offset, max(0, len(wrapped)-rows))
		var screen strings.Builder
		screen.WriteString("\x1b[H\x1b[2J" + fit(title, width-1) + "\r\n\r\n")
		visible := wrapped[offset:min(len(wrapped), offset+rows)]
		for i, row := range visible {
			screen.WriteString(row.text)
			if !row.joined || i == len(visible)-1 {
				screen.WriteString("\r\n")
			}
		}
		screen.WriteString("\r\n" + fit(fmt.Sprintf("Lines %d-%d of %d | j/k/Space", offset+1, min(len(wrapped), offset+rows), len(wrapped)), width-1) + "\r\n" + fit("Enter/Esc: back | Ctrl+C: quit", width-1))
		if screen.String() != previous {
			if _, err := fmt.Fprint(c.output, screen.String()); err != nil {
				return err
			}
			previous = screen.String()
		}
		pressed, err := readKey(ctx, c.input)
		if err != nil {
			return err
		}
		switch pressed {
		case keyBack, keyEnter:
			return nil
		case keyQuit:
			return ErrCancelled
		case keyUp:
			offset = max(0, offset-1)
		case keyDown:
			offset++
		case keyToggle:
			offset += rows
		}
	}
}

type confirmRow struct {
	text   string
	choice int
	action string
}

const maxHighlights = 8

func confirmRows(catalog engine.Catalog, proposal engine.Proposal) []confirmRow {
	rows := []confirmRow{}
	highlights := planHighlights(catalog, proposal.Plan)
	if len(highlights) > maxHighlights {
		highlights = append(highlights[:maxHighlights-1], fmt.Sprintf("+%d more | d: details", len(highlights)-maxHighlights+1))
	}
	for _, text := range highlights {
		rows = append(rows, confirmRow{text: text, choice: -1})
	}
	group := ""
	for i, choice := range proposal.Choices {
		if choice.Group != group {
			group = choice.Group
			rows = append(rows, confirmRow{choice: -1}, confirmRow{text: group + ":", choice: -1})
		}
		mark := "[ ] "
		if choice.Checked {
			mark = "[x] "
		}
		rows = append(rows, confirmRow{text: mark + choice.Label, choice: i})
	}
	return append(rows, confirmRow{choice: -1}, confirmRow{text: "Apply changes", choice: -1, action: "apply"}, confirmRow{text: "Back to main menu", choice: -1, action: "back"})
}

func (row confirmRow) selectable() bool { return row.choice >= 0 || row.action != "" }

func (c *Console) confirm(ctx context.Context, catalog engine.Catalog, proposal engine.Proposal, revise engine.Revise) (accepted bool, resultErr error) {
	resultErr = c.interact(func() error {
		if err := discardPending(ctx, c.input); err != nil {
			return err
		}
		rows := confirmRows(catalog, proposal)
		cursor := slices.IndexFunc(rows, func(row confirmRow) bool { return row.action == "apply" })
		offset, previous, working := 0, "", ""
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			width, height, err := term.GetSize(int(c.output.Fd()))
			if err != nil {
				return err
			}
			if width < 32 || height < 10 {
				return errors.New("enlarge the terminal to at least 32 columns and 10 rows, then try again")
			}
			top := append([]string{"Apply these changes?"}, planSummary(proposal.Plan)...)
			if proposal.Notice != "" {
				top = append(top, proposal.Notice)
			}
			hint := "j/k: move  Enter: choose"
			if len(proposal.Choices) != 0 {
				hint = "j/k: move  Space: toggle  Enter: choose"
			}
			detail := working
			if detail == "" && rows[cursor].choice >= 0 {
				detail = proposal.Choices[rows[cursor].choice].Detail
			}
			bottom := []string{detail, hint, "d: details  Esc: back  ^C: quit"}
			region := max(1, height-len(top)-len(bottom))
			if cursor < offset {
				offset = cursor
			}
			if cursor >= offset+region {
				offset = cursor - region + 1
			}
			if first := slices.IndexFunc(rows, confirmRow.selectable); cursor == first && cursor < region {
				offset = 0
			}
			lines := slices.Clone(top)
			for i := offset; i < min(len(rows), offset+region); i++ {
				pointer := "  "
				if i == cursor {
					pointer = "> "
				}
				if !rows[i].selectable() {
					pointer = ""
				}
				lines = append(lines, pointer+rows[i].text)
			}
			for len(lines) < len(top)+region {
				lines = append(lines, "")
			}
			lines = append(lines, bottom...)
			for i := range lines {
				lines[i] = fit(lines[i], width-1)
			}
			screen := "\x1b[H\x1b[2J" + strings.Join(lines, "\r\n")
			if screen != previous {
				if _, err := fmt.Fprint(c.output, screen); err != nil {
					return err
				}
				previous = screen
			}
			if working != "" {
				choices := slices.Clone(proposal.Choices)
				index := rows[cursor].choice
				choices[index].Checked = !choices[index].Checked
				var revised engine.Proposal
				if err := c.cooked(ctx, func() (err error) {
					revised, err = revise(ctx, choices)
					return err
				}); err != nil {
					return err
				}
				proposal, working = revised, ""
				focus := choices[index]
				rows = confirmRows(catalog, proposal)
				cursor = slices.IndexFunc(rows, func(row confirmRow) bool {
					return row.choice >= 0 && proposal.Choices[row.choice].Group == focus.Group && proposal.Choices[row.choice].ID == focus.ID
				})
				if cursor < 0 {
					cursor = slices.IndexFunc(rows, func(row confirmRow) bool { return row.action == "apply" })
				}
				continue
			}
			pressed, err := readKey(ctx, c.input)
			if err != nil {
				return err
			}
			move := func(step int) {
				for next := cursor + step; next >= 0 && next < len(rows); next += step {
					if rows[next].selectable() {
						cursor = next
						return
					}
				}
			}
			switch pressed {
			case keyBack:
				return ErrBack
			case keyQuit:
				return ErrCancelled
			case keyUp:
				move(-1)
			case keyDown:
				move(1)
			case keyDetails:
				if err := c.read(ctx, "Review every change", planText(catalog, proposal.Plan, proposal.Details)); err != nil {
					return err
				}
				previous = ""
			case keyToggle, keyEnter:
				switch {
				case rows[cursor].choice >= 0 && revise != nil:
					working = "Updating the plan..."
				case pressed == keyEnter && rows[cursor].action == "apply":
					accepted = true
					return nil
				case pressed == keyEnter && rows[cursor].action == "back":
					return ErrBack
				}
			}
		}
	})
	return accepted, resultErr
}

func plain(text string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || r >= 0x80 && r < 0xa0 {
			return -1
		}
		return r
	}, text)
}

func wrapLines(lines []string, cells int) []string {
	result := []string{}
	for _, line := range lines {
		clean := plain(line)
		for len(clean) > 0 {
			part := fit(clean, cells)
			result = append(result, part)
			clean = clean[len(part):]
		}
		if line == "" {
			result = append(result, "")
		}
	}
	return result
}
