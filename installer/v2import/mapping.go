package v2import

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/lgmr-dev/dotfiles/installer/catalog"
	"github.com/lgmr-dev/dotfiles/installer/engine"
	"github.com/lgmr-dev/dotfiles/installer/hooks"
	"github.com/lgmr-dev/dotfiles/installer/platform"
	"github.com/lgmr-dev/dotfiles/installer/state"
	"github.com/lgmr-dev/dotfiles/installer/system"
	"github.com/lgmr-dev/dotfiles/installer/targets"
)

// Options is what the import and restore-v2 need from the wiring. Engine is
// the wired engine of the installation: the import uses its catalog, router,
// state folder, target and hooks. Folders holds the known folders. Kinds are
// the kinds that the providers add to a targets.Writer. KeyboardRule is the
// path of the udev rule, system.KeyboardRulePath outside tests. VerifyNvim,
// when set, starts Neovim through the switched runtime link and fails when
// Neovim does not start.
type Options struct {
	Engine       *engine.Engine
	Folders      platform.Folders
	Kinds        map[string]targets.Kind
	KeyboardRule string
	VerifyNvim   func(ctx context.Context) error
}

// Row is one line of the mapping table: what the import does with one
// receipt or one target of release v2.
type Row struct {
	Resource, Target, Result string
}

// Mapping is the result of the read-only mapping of a schema-1 state. State
// is the schema-2 state in memory with v2_restore "switching". Original
// holds the exact bytes of the schema-1 file. Deferred lists the resources
// that the import run leaves out of its closure: the macOS applications that
// stay on their payloads of release v2, their desktop integrations, and each
// requirement that only they have and that release v2 did not record.
// Legacy lists the command folders of those payloads.
type Mapping struct {
	State    state.State
	Original []byte
	Rows     []Row
	Notes    []string
	Deferred []string
	Legacy   []string
}

type memory struct{ st state.State }

func (m *memory) State() state.State { return m.st.Clone() }

func (m *memory) Update(edit func(*state.State) error) error {
	next := m.st.Clone()
	if next.Targets == nil {
		next.Targets = map[string]state.Record{}
	}
	if err := edit(&next); err != nil {
		return err
	}
	m.st = next
	return nil
}

type mapper struct {
	o       Options
	catalog engine.Catalog
	ctx     platform.Context
	dir     string
	v1      State
	st      state.State
	writer  *targets.Writer
	out     Mapping
}

const (
	terminalResource = "integration.windows-terminal"
	vscodeResource   = "integration.vscode-managed"
	keyboardResource = "system.keyboard-access"
)

type deferredApplication struct{ tool, desktop, bundle, bin, executable string }

var deferredApplications = []deferredApplication{
	{"tool.vscode", "desktop.vscode", "Visual Studio Code.app", "Visual Studio Code.app/Contents/Resources/app/bin", "Visual Studio Code.app/Contents/MacOS/Code"},
	{"tool.ghostty", "integration.ghostty", "Ghostty.app", "Ghostty.app/Contents/MacOS", "Ghostty.app/Contents/MacOS/ghostty"},
	{"tool.openwhispr", "desktop.openwhispr", "OpenWhispr.app", "", "OpenWhispr.app/Contents/MacOS/OpenWhispr"},
}

func within(folder, path string) bool {
	relative, err := filepath.Rel(folder, path)
	return err == nil && filepath.IsAbs(path) && (relative == "." || filepath.IsLocal(relative))
}

func (o Options) writer() *targets.Writer {
	return &targets.Writer{StateDir: o.Engine.StateDir, Kinds: o.Kinds}
}

func (o Options) keyboardRule() string {
	if o.KeyboardRule != "" {
		return o.KeyboardRule
	}
	return system.KeyboardRulePath
}

func (o Options) validate() error {
	if o.Engine == nil || o.Engine.Catalog == nil || o.Engine.Router == nil || !filepath.IsAbs(o.Engine.StateDir) || !filepath.IsAbs(o.Folders.Home) {
		return errors.New("the import from release v2 requires the wired engine and the home folder")
	}
	return o.Engine.Context.Validate()
}

func (m *mapper) row(resource, target, result string) {
	m.out.Rows = append(m.out.Rows, Row{resource, target, result})
}

func (m *mapper) note(format string, arguments ...any) {
	m.out.Notes = append(m.out.Notes, fmt.Sprintf(format, arguments...))
}

func (m *mapper) record(key string, record state.Record, owned bool) {
	result := "baseline only, no ownership: setup asks before it replaces the content"
	if owned {
		result = "owned " + record.Kind
	}
	if backup, ok := strings.CutPrefix(record.Before, "backup:"); ok {
		result += ", first baseline stays at " + backup
	} else if record.Before != "" && record.Before != targets.Absent {
		result += ", first baseline kept"
	}
	m.st.Targets[key] = record
	m.row(record.Resource, key, result)
}

func recordKind(snapshot string) string {
	switch snapshot {
	case "link", "junction":
		return "link"
	case "directory":
		return "tree"
	case "file":
		return "file"
	}
	return ""
}

func (m *mapper) created(receipt Receipt) bool {
	return receipt.Ownership == "created" && receipt.After.Present
}

func (m *mapper) configuration(id string, receipt Receipt) error {
	baseline, err := readConfigBaseline(m.dir, id, receipt)
	if err != nil {
		if receipt.Ownership == "created" {
			return refused("cannot use the saved baseline of %s: %v", id, err)
		}
		m.note("%s: release v2 did not own it and its baseline is not readable; setup treats its content as yours.", id)
		return nil
	}
	type observed struct {
		Path     string
		Snapshot platform.Snapshot
	}
	originals, paths, backups := []observed{}, []string{}, []string{}
	for _, target := range baseline.Targets {
		originals, paths, backups = append(originals, observed{target.Destination, target.Original}), append(paths, target.Destination), append(backups, target.Backup)
	}
	originalsDigest, originalsErr := digest(originals)
	pathsDigest, pathsErr := digest(paths)
	if err := errors.Join(originalsErr, pathsErr); err != nil {
		return err
	}
	verified := originalsDigest == receipt.Before.Fingerprint && pathsDigest == receipt.Before.Identity
	if !verified {
		m.note("%s: the saved baseline %s is not the one that release v2 recorded. Setup cannot prove the earlier content of %s, so it takes no ownership of this resource and asks before it replaces its content. The earlier content that release v2 saved stays at %s.",
			id, filepath.Join(m.dir, receipt.Recovery, "configuration.json"), strings.Join(paths, ", "), strings.Join(backups, ", "))
	}
	actual, readable := []observed{}, true
	for _, target := range baseline.Targets {
		snapshot, err := platform.SnapshotTree(target.Destination)
		if err != nil {
			readable = false
			m.note("%s: cannot read %s: %v. Setup takes no ownership of this resource.", id, target.Destination, err)
		}
		actual = append(actual, observed{target.Destination, snapshot})
	}
	fingerprint, err := digest(actual)
	if err != nil {
		return err
	}
	owned := verified && m.created(receipt) && readable && fingerprint == receipt.After.Fingerprint
	for i, target := range baseline.Targets {
		live := actual[i].Snapshot
		kind := recordKind(live.Kind)
		if kind == "" {
			kind = recordKind(target.Original.Kind)
		}
		if kind == "" || owned && live.Kind == "absent" {
			m.row(id, target.Destination, "absent now and before: no record")
			continue
		}
		record := state.Record{Resource: id, Kind: kind, Before: targets.Absent, Made: slices.Clone(target.Created)}
		unitOwned := owned
		switch {
		case !verified:
			record.Made, record.Before = nil, "fp:"+receipt.Before.Fingerprint
		case target.Original.Kind != "absent":
			record.Made = nil
			record.Before = "backup:" + target.Backup
			if backup, err := platform.SnapshotTree(target.Backup); err != nil || backup != target.Original {
				record.Before, unitOwned = "fp:"+target.Original.Kind+":"+target.Original.Hash, false
				m.note("%s: the earlier content of %s, which release v2 saved at %s, is missing or changed. Setup cannot give it back, so it takes no ownership of %s and asks before it replaces it.", id, target.Destination, target.Backup, target.Destination)
			}
		}
		if unitOwned {
			record.Applied = live.Kind + ":" + live.Hash
		}
		m.record(target.Destination, record, unitOwned)
	}
	if id == hooks.FontResource && owned {
		m.fontRegistrations(id, receipt, baseline)
	}
	return nil
}

func registeredByV2(receipt Receipt, paths []string) bool {
	empty, emptyErr := digest(make([]string, len(paths)))
	registered, registeredErr := digest(paths)
	return errors.Join(emptyErr, registeredErr) == nil && receipt.Before.Inventory == empty && receipt.After.Inventory == registered
}

func (m *mapper) fontRegistration(id, path string) (targets.Live, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return targets.Live{}, err
	}
	sum := sha256.Sum256(data)
	live, err := m.writer.Inspect(targets.Desired{Resource: id, Kind: "font", Path: path, Slot: hooks.FontSlot, Content: []byte(hex.EncodeToString(sum[:]))}, &memory{})
	if err == nil && live.State == targets.StateUnreadable {
		err = errors.New(live.Detail)
	}
	return live, err
}

func (m *mapper) fontRegistrations(id string, receipt Receipt, baseline configBaseline) {
	if _, ok := m.o.Kinds["font"]; !ok {
		m.note("%s: this build has no font registration reader; setup inspects the registrations again.", id)
		return
	}
	paths := []string{}
	for _, target := range baseline.Targets {
		paths = append(paths, target.Destination)
	}
	windows := m.ctx.OS == "windows"
	made := !windows || registeredByV2(receipt, paths)
	for _, path := range paths {
		key := targets.Key(path, hooks.FontSlot)
		live, err := m.fontRegistration(id, path)
		if err != nil {
			m.note("%s: cannot read the font registration of %s: %v", id, path, err)
			continue
		}
		if live.Fingerprint == targets.Absent {
			m.row(id, key, "no active registration: no record")
			continue
		}
		if made && (!windows || live.Fingerprint == live.Want) {
			m.record(key, state.Record{Resource: id, Kind: "font", Applied: live.Fingerprint, Before: targets.Absent}, true)
			continue
		}
		m.note("%s: the font registration %s is not the registration of %s that release v2 made. Setup takes no ownership of it and does not change it.", id, key, path)
		m.record(key, state.Record{Resource: id, Kind: "font", Before: "fp:" + live.Fingerprint}, false)
	}
}

func inlineValue(value []byte) string {
	return "value:" + base64.StdEncoding.EncodeToString(value)
}

func (m *mapper) profile(id string, receipt Receipt) error {
	baseline, err := readProfileBaseline(m.dir, id, receipt)
	if err != nil {
		if receipt.Ownership == "created" {
			return refused("cannot use the saved baseline of %s: %v", id, err)
		}
		m.note("%s: release v2 did not own it and its baseline is not readable; setup treats its content as yours.", id)
		return nil
	}
	if err := verifyProfileBaseline(id, receipt, baseline); err != nil {
		m.note("%s: the saved baseline %s is not the one that release v2 recorded: %v. Setup cannot give the earlier values back, so it takes no ownership of this resource and asks before it replaces its content.",
			id, filepath.Join(m.dir, receipt.Recovery, "profile.json"), err)
		m.unverified(id, receipt, baseline)
		return nil
	}
	switch {
	case len(baseline.Fields) == 0:
		return m.blocks(id, receipt, baseline)
	case id == terminalResource:
		return m.terminal(id, receipt, baseline)
	}
	return m.fields(id, receipt, baseline)
}

func (m *mapper) unverified(id string, receipt Receipt, baseline profileBaseline) {
	unowned := func(key string) {
		m.record(key, state.Record{Resource: id, Kind: "field", Before: "fp:" + receipt.Before.Fingerprint}, false)
	}
	for _, entry := range baseline.Entries {
		switch {
		case id == terminalResource:
			unowned(targets.Key(entry.Path, hooks.TerminalSlot))
		case len(baseline.Fields) != 0:
			for _, field := range baseline.Fields {
				unowned(targets.Key(entry.Path, field))
			}
		case within(filepath.Join(m.dir, "shells"), entry.Path):
			m.row(id, entry.Path, "generated file without proof of ownership: no record")
		default:
			m.record(targets.Key(entry.Path, id), state.Record{Resource: id, Kind: "block", Before: "fp:" + receipt.Before.Fingerprint}, false)
		}
	}
}

func (m *mapper) blocks(id string, receipt Receipt, baseline profileBaseline) error {
	blocks, contents, files, exact, readable := [][]byte{}, [][]byte{}, []platform.Snapshot{}, []bool{}, true
	for _, entry := range baseline.Entries {
		data, file, err := readProfileFile(entry.Path)
		var block []byte
		if err == nil {
			block, err = liveBlock(data, id)
		}
		exact = append(exact, block != nil && bytes.Equal(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf}), block))
		if err != nil {
			readable = false
			m.note("%s: cannot read the managed block in %s: %v. Setup takes no ownership of this resource.", id, entry.Path, err)
		}
		blocks, contents, files = append(blocks, block), append(contents, data), append(files, file)
	}
	fingerprint, err := digest(blocks)
	if err != nil {
		return err
	}
	owned := m.created(receipt) && readable && fingerprint == receipt.After.Fingerprint
	for i, entry := range baseline.Entries {
		if within(filepath.Join(m.dir, "shells"), entry.Path) {
			snapshot := files[i]
			if owned && entry.Absent && snapshot.Kind == "file" && !exact[i] {
				m.row(id, entry.Path, "generated file with content outside the block of release v2: no record, and setup asks before it replaces the file")
				m.note("%s: %s holds lines outside the block that release v2 wrote. Setup keeps the file. It saves the file before a replacement that you approve.", id, entry.Path)
				continue
			}
			if !owned || !entry.Absent || snapshot.Kind != "file" {
				m.row(id, entry.Path, "generated file without proof of ownership: no record")
				continue
			}
			m.record(entry.Path, state.Record{Resource: id, Kind: "file", Applied: "file:" + snapshot.Hash, Before: targets.Absent, Made: slices.Clone(entry.Created)}, true)
			continue
		}
		key := targets.Key(entry.Path, id)
		record := state.Record{Resource: id, Kind: "block", Before: targets.Absent}
		switch {
		case entry.Absent:
			record.Made = append([]string{entry.Path}, entry.Created...)
		case len(entry.Block) != 0:
			value, present, err := targets.Block{}.Extract(entry.Block, targets.Desired{Slot: id})
			if err != nil || !present {
				m.note("%s: the earlier managed block of %s is not readable; a removal deletes the block and does not give the earlier one back.", id, entry.Path)
				break
			}
			record.Before = inlineValue(value)
		}
		slotOwned := false
		if owned && blocks[i] != nil {
			live, err := m.writer.ObserveContent(key, record, contents[i])
			if err == nil && live != targets.Absent {
				record.Applied, slotOwned = live, true
			}
		}
		m.record(key, record, slotOwned)
	}
	return nil
}

func (m *mapper) fields(id string, receipt Receipt, baseline profileBaseline) error {
	if len(baseline.Entries) != 1 {
		return refused("the saved baseline of %s has more than one settings file", id)
	}
	entry := baseline.Entries[0]
	slot := targets.Field{JSONC: func(string) bool { return id == vscodeResource }}
	data, file, err := readProfileFile(entry.Path)
	values, readable := map[string]json.RawMessage{}, err == nil
	if file.Kind != "absent" && readable && len(data) != 0 {
		for _, field := range baseline.Fields {
			value, present, err := slot.Extract(data, targets.Desired{Path: entry.Path, Slot: field})
			if err != nil {
				readable = false
				m.note("%s: cannot read the field %s in %s: %v. Setup takes no ownership of this resource.", id, field, entry.Path, err)
				break
			}
			if present {
				values[field] = value
			}
		}
	}
	var block []byte
	if len(values) != 0 {
		if block, err = json.Marshal(values); err != nil {
			return err
		}
	}
	fingerprint, err := digest([][]byte{block})
	if err != nil {
		return err
	}
	owned := m.created(receipt) && readable && fingerprint == receipt.After.Fingerprint
	earlier := map[string]json.RawMessage{}
	if len(entry.Block) != 0 {
		if err := json.Unmarshal(entry.Block, &earlier); err != nil {
			return refused("the saved baseline of %s is not a JSON object: %v", id, err)
		}
	}
	first := true
	for _, field := range baseline.Fields {
		key := targets.Key(entry.Path, field)
		record := state.Record{Resource: id, Kind: "field", Before: targets.Absent}
		raw, had := earlier[field]
		if had {
			value, err := slot.Render(targets.Desired{Slot: field, Content: raw})
			if err != nil {
				return refused("the saved baseline of %s has an invalid value for %s: %v", id, field, err)
			}
			record.Before = inlineValue(value)
		}
		_, live := values[field]
		slotOwned := owned && live
		if !slotOwned && !had {
			m.row(id, key, "no value now or before: no record")
			continue
		}
		if slotOwned {
			if record.Applied, err = m.writer.ObserveContent(key, record, data); err != nil {
				return refused("cannot read %s: %v", key, err)
			}
		}
		if first && entry.Absent {
			record.Made = append([]string{entry.Path}, entry.Created...)
		}
		first = false
		m.record(key, record, slotOwned)
	}
	return nil
}

func (m *mapper) terminal(id string, receipt Receipt, baseline profileBaseline) error {
	if len(baseline.Entries) != 1 {
		return refused("the saved baseline of %s has more than one settings file", id)
	}
	entry := baseline.Entries[0]
	key := targets.Key(entry.Path, hooks.TerminalSlot)
	record := state.Record{Resource: id, Kind: "field", Before: targets.Absent}
	if len(entry.Block) != 0 {
		record.Before = inlineValue(entry.Block)
	}
	if entry.Absent {
		record.Made = append([]string{entry.Path}, entry.Created...)
	}
	owned := false
	if data, file, err := readProfileFile(entry.Path); err == nil && file.Kind != "absent" && m.created(receipt) {
		if fingerprint, err := hooks.TerminalImportFingerprint(data); err == nil && fingerprint != "" && fingerprint == receipt.After.Fingerprint {
			if live, err := m.writer.ObserveContent(key, record, data); err == nil && live != targets.Absent {
				record.Applied, owned = live, true
			}
		}
	}
	if !owned && record.Before == targets.Absent {
		m.row(id, key, "no reviewed value before and no proof of ownership: no record")
		return nil
	}
	m.record(key, record, owned)
	return nil
}

var keyboardHeader = regexp.MustCompile(`\A# Managed by dotfiles \(Mac keyboard shortcuts\), operation ([0-9a-f]{64})\.\n`)

const keyboardBody = "# Lets the user logged in at this seat read keyboards and create a virtual\n" +
	"# keyboard for the xremap key remapper. Unselect the tool to remove it.\n" +
	`KERNEL=="uinput", SUBSYSTEM=="misc", TAG+="uaccess", OPTIONS+="static_node=uinput"
SUBSYSTEM=="input", KERNEL=="event*", ENV{ID_INPUT_KEYBOARD}=="1", TAG+="uaccess"
`

func keyboardOwner(data []byte) bool {
	match := keyboardHeader.Find(data)
	return match != nil && string(data[len(match):]) == keyboardBody
}

func (m *mapper) keyboard(id string, receipt Receipt) {
	path := m.o.keyboardRule()
	data, _, err := readProfileFile(path)
	sum := sha256.Sum256(data)
	if err != nil || !m.created(receipt) || !keyboardOwner(data) || hex.EncodeToString(sum[:]) != receipt.After.Fingerprint {
		m.row(id, path, "not the exact rule that release v2 installed: no record, and setup does not replace it")
		return
	}
	m.record(path, state.Record{Resource: id, Kind: "file", Applied: system.KeyboardRuleFingerprint(data), Before: targets.Absent}, true)
}

func (m *mapper) neovim(id string, receipt Receipt) {
	link := receipt.After.Identity
	if !cleanAbsolute(link) || filepath.Base(link) != "current" || within(m.dir, link) {
		m.row(id, link, "no record: setup builds a new runtime")
		return
	}
	target, snapshot, err := readLink(link)
	fingerprint, _ := digest(target)
	if err != nil || !m.created(receipt) || fingerprint != receipt.After.Fingerprint {
		m.row(id, link, "not the runtime link that release v2 published: no record, and setup asks before it replaces the link")
		return
	}
	m.record(link, state.Record{Resource: id, Kind: "link", Applied: snapshot.Kind + ":" + snapshot.Hash, Before: targets.Absent}, true)
}

func (m *mapper) sentinel(id string) error {
	checkout := filepath.Join(m.dir, "sentinel", "checkout")
	fingerprint, err := hooks.SentinelCheckoutFingerprint(checkout)
	if err != nil {
		return refused("cannot read the Sentinel checkout %s: %v", checkout, err)
	}
	if fingerprint == targets.Absent {
		m.row(id, checkout, "record.json stays; no checkout")
		return nil
	}
	key := hooks.SentinelCheckoutKey(m.dir)
	m.st.Targets[key] = state.Record{Resource: hooks.SentinelResource, Kind: "tree", Before: "fp:" + fingerprint}
	m.row(id, key, "record.json stays; checkout recorded without ownership: the first replacement moves it aside")
	return nil
}

func systemRecord(r catalog.Resource, ctx platform.Context, provider string) (key, name string, ok bool) {
	if key, name, ok = engine.SystemRecord(r, ctx); ok {
		return key, name, true
	}
	if ctx.OS == "windows" && (provider == "windows-vendor" || provider == "windows-buildtools" || provider == "windows-app") {
		return "windows", r.ID, true
	}
	return "", "", false
}

func (m *mapper) present(ctx context.Context, r catalog.Resource) (bool, error) {
	provider, err := m.o.Engine.Router(r)
	if err != nil || provider == nil {
		return false, err
	}
	finding, err := provider.Inspect(ctx, r, engine.Want{Present: true}, &memory{m.st.Clone()})
	if err != nil {
		return false, err
	}
	return finding.Status == engine.Satisfied || finding.Status == engine.Outdated || finding.Status == engine.Unhealthy || finding.Status == engine.Changed, nil
}

func (m *mapper) system(ctx context.Context, r catalog.Resource, receipt Receipt) error {
	key, name, ok := systemRecord(r, m.ctx, receipt.After.Provider)
	if !ok {
		m.row(r.ID, "", "no record in release v3: setup inspects it")
		return nil
	}
	if !m.created(receipt) {
		m.row(r.ID, key+":"+name, "present before release v2: no record, and setup never changes it")
		return nil
	}
	present, err := m.present(ctx, r)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil || !present {
		m.row(r.ID, key+":"+name, "not present now, or setup cannot inspect it: no record")
		return nil
	}
	m.st.AddSystem(key, name)
	m.row(r.ID, key+":"+name, "recorded as installed by setup")
	return nil
}

func (m *mapper) receipt(ctx context.Context, id string, receipt Receipt) error {
	if retiredSentinel(id) {
		m.row(id, "", "retired Sentinel record: dropped")
		return nil
	}
	r, known := m.catalog.Resource(id)
	if !known || !r.Available(m.ctx) {
		if receipt.Ownership == "created" {
			return refused("the state of release v2 records %s, which this release does not have on this system", id)
		}
		m.row(id, "", "unknown to this release and not owned: dropped")
		return nil
	}
	provider := receipt.After.Provider
	if provider == "" {
		provider = receipt.Before.Provider
	}
	switch {
	case id == hooks.SentinelResource || strings.HasPrefix(provider, "sentinel"):
		return m.sentinel(id)
	case provider == "archive":
		m.row(id, filepath.Join(m.dir, "packages", id), "private payload: setup builds it again under tools, and the folder of release v2 stays")
		return nil
	case provider == "nvim-sync":
		m.neovim(id, receipt)
		return nil
	case provider == "keyboard-access":
		m.keyboard(id, receipt)
		return nil
	case baselineExists(m.dir, receipt, "configuration.json"):
		return m.configuration(id, receipt)
	case baselineExists(m.dir, receipt, "profile.json"):
		return m.profile(id, receipt)
	case receipt.Ownership == "created" && slices.Contains([]string{"configuration", "profile-block"}, provider):
		return refused("the saved baseline of %s is missing", id)
	}
	return m.system(ctx, r, receipt)
}

// LegacyCommandDirectories returns the command folders of the macOS
// applications that still run from their payloads of release v2: an
// application counts while a recorded link of its desktop integration points
// into the packages folder of release v2. The shell profile keeps these
// folders on PATH until the cask of the application verifies and the desktop
// integration releases that link.
func LegacyCommandDirectories(stateDir string, st state.State, ctx platform.Context) []string {
	return legacyDirectories(stateDir, legacyTools(stateDir, st, ctx))
}

// LegacyApplications returns the macOS applications that still run from
// their payloads of release v2: tool.vscode, tool.ghostty or tool.openwhispr.
// An application counts while a recorded link of its desktop integration
// points into the packages folder of release v2 and the application can
// start: its desktop integration has an owned record of the link
// ~/Applications/<App>.app, that link resolves to the bundle in the current
// payload, each other recorded link of the integration, such as the code
// command of VS Code, resolves to an executable file, and the bundle
// executable is a regular executable file.
func LegacyApplications(stateDir string, st state.State, ctx platform.Context) []string {
	return slices.DeleteFunc(legacyTools(stateDir, st, ctx), func(tool string) bool {
		return !runnable(stateDir, st, tool)
	})
}

func runnable(stateDir string, st state.State, tool string) bool {
	index := slices.IndexFunc(deferredApplications, func(application deferredApplication) bool { return application.tool == tool })
	if index < 0 {
		return false
	}
	application := deferredApplications[index]
	current := filepath.Join(stateDir, "packages", tool, "current")
	launcher := false
	for key, record := range st.Targets {
		if record.Resource != application.desktop || record.Kind != "link" {
			continue
		}
		info, err := os.Stat(key)
		if err != nil {
			return false
		}
		if filepath.Base(key) != application.bundle || filepath.Base(filepath.Dir(key)) != "Applications" {
			if !executableFile(info) {
				return false
			}
			continue
		}
		target, err := os.Readlink(key)
		if err != nil || target != filepath.Join(current, application.bundle) || record.Applied == "" || !info.IsDir() {
			return false
		}
		launcher = true
	}
	info, err := os.Stat(filepath.Join(current, filepath.FromSlash(application.executable)))
	return launcher && err == nil && executableFile(info)
}

func executableFile(info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0
}

func legacyDirectories(stateDir string, tools []string) []string {
	directories := []string{}
	for _, application := range deferredApplications {
		if slices.Contains(tools, application.tool) && application.bin != "" {
			directories = append(directories, filepath.Join(stateDir, "packages", application.tool, "current", filepath.FromSlash(application.bin)))
		}
	}
	return directories
}

func legacyTools(stateDir string, st state.State, ctx platform.Context) []string {
	tools := []string{}
	if ctx.OS != "darwin" {
		return tools
	}
	for _, key := range slices.Sorted(maps.Keys(st.Targets)) {
		if st.Targets[key].Kind != "link" {
			continue
		}
		target, err := os.Readlink(key)
		if err != nil {
			continue
		}
		for _, application := range deferredApplications {
			if within(filepath.Join(stateDir, "packages", application.tool), target) && !slices.Contains(tools, application.tool) {
				tools = append(tools, application.tool)
			}
		}
	}
	return tools
}

func deferredResources(c engine.Catalog, ctx platform.Context, tools []string) []string {
	deferred := slices.Clone(tools)
	for _, application := range deferredApplications {
		id := application.desktop
		r, known := c.Resource(id)
		if !known || !r.Available(ctx) {
			continue
		}
		closure, err := c.Closure([]string{id}, ctx)
		if err == nil && slices.ContainsFunc(tools, func(tool string) bool { return slices.Contains(closure, tool) }) {
			deferred = append(deferred, id)
		}
	}
	slices.Sort(deferred)
	return deferred
}

func hiddenResources(c engine.Catalog, ctx platform.Context, st state.State, deferred []string) ([]string, error) {
	if len(deferred) == 0 {
		return nil, nil
	}
	roots := slices.Concat(st.Selected, st.Keep)
	full, err := c.Closure(roots, ctx)
	if err != nil {
		return nil, err
	}
	reached := map[string]bool{}
	var visit func(id string)
	visit = func(id string) {
		if reached[id] || slices.Contains(deferred, id) {
			return
		}
		reached[id] = true
		if r, known := c.Resource(id); known {
			for _, dependency := range r.Dependencies(ctx) {
				visit(dependency)
			}
		}
	}
	for _, root := range roots {
		visit(root)
	}
	for range full {
		for _, id := range full {
			if r, known := c.Resource(id); known && !reached[id] && engine.Recorded(st, r, ctx) {
				visit(id)
			}
		}
	}
	hidden := slices.Clone(deferred)
	for _, id := range full {
		if !reached[id] {
			hidden = append(hidden, id)
		}
	}
	return sortedUnique(hidden), nil
}

func sortedUnique(values []string) []string {
	result := slices.Clone(values)
	slices.Sort(result)
	result = slices.Compact(result)
	if result == nil {
		result = []string{}
	}
	return result
}

// Map reads the schema-1 state of the installation and maps it to a
// schema-2 state in memory. It inspects the live targets and the system
// items, and it writes nothing.
func Map(ctx context.Context, o Options) (Mapping, error) {
	if err := o.validate(); err != nil {
		return Mapping{}, err
	}
	e := o.Engine
	v1, original, err := LoadState(e.StateDir, o.Folders.Home, e.Context)
	if err != nil {
		return Mapping{}, err
	}
	m := &mapper{o: o, catalog: e.Catalog, ctx: e.Context, dir: e.StateDir, v1: v1, writer: o.writer(),
		st: state.State{Schema: 2, Target: e.Target, V2Restore: "switching", System: map[string][]string{}, Targets: map[string]state.Record{}}}
	m.out.Original = original
	for _, id := range v1.Selected {
		if r, known := m.catalog.Resource(id); !known || !r.Capability || !r.Available(m.ctx) {
			return Mapping{}, refused("the state of release v2 selects %s, which this release does not offer on this system", id)
		}
	}
	for _, id := range v1.retired {
		m.row(id, "", "retired in release v2: its record is dropped and its files stay")
	}
	keep := []string{}
	for _, id := range v1.Keep {
		if retiredSentinel(id) {
			continue
		}
		if r, known := m.catalog.Resource(id); !known || !r.Available(m.ctx) {
			return Mapping{}, refused("the state of release v2 keeps %s, which this release does not have on this system", id)
		}
		keep = append(keep, id)
	}
	for _, id := range slices.Sorted(maps.Keys(v1.Receipts)) {
		if err := ctx.Err(); err != nil {
			return Mapping{}, err
		}
		if err := m.receipt(ctx, id, v1.Receipts[id]); err != nil {
			return Mapping{}, err
		}
	}
	closure, err := m.catalog.Closure(slices.Concat(v1.Selected, keep), m.ctx)
	if err != nil {
		return Mapping{}, refused("this release cannot plan the selection of release v2: %v", err)
	}
	for _, id := range slices.Sorted(maps.Keys(v1.Receipts)) {
		receipt := v1.Receipts[id]
		r, known := m.catalog.Resource(id)
		if !known || retiredSentinel(id) || !m.created(receipt) || slices.Contains(closure, id) || r.Action == "" {
			continue
		}
		if _, _, bound := systemRecord(r, m.ctx, receipt.After.Provider); bound {
			continue
		}
		owned := false
		for _, record := range m.st.Targets {
			owned = owned || record.Resource == id && record.Applied != ""
		}
		if owned && (r.Retain || r.Shared) {
			m.row(id, "", "retained by the rules of release v3")
			continue
		}
		keep = append(keep, id)
		m.row(id, "", "release v2 retained it and no rule of release v3 does: added to the kept resources")
	}
	m.st.Selected, m.st.Keep = sortedUnique(v1.Selected), sortedUnique(keep)
	for provider, names := range m.st.System {
		m.st.System[provider] = sortedUnique(names)
	}
	if err := m.st.Validate(); err != nil {
		return Mapping{}, refused("the mapped state is invalid: %v", err)
	}
	m.out.State = m.st
	tools := legacyTools(m.dir, m.st, m.ctx)
	for _, application := range deferredApplications {
		receipt, has := v1.Receipts[application.tool]
		if m.ctx.OS == "darwin" && has && receipt.After.Provider == "archive" && !slices.Contains(tools, application.tool) {
			tools = append(tools, application.tool)
		}
	}
	if m.out.Deferred, err = hiddenResources(m.catalog, m.ctx, m.st, deferredResources(m.catalog, m.ctx, tools)); err != nil {
		return Mapping{}, refused("this release cannot plan the selection of release v2: %v", err)
	}
	m.out.Legacy = legacyDirectories(m.dir, tools)
	return m.out, nil
}
