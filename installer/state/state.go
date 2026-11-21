// Package state holds the strict schema-2 installer state: the selection, the
// system package records, the pending marks and the target records.
package state

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/luisgui1757/dotfiles/installer/platform"
)

// File and LockFile are the names of the state document and the installer
// lock in the state folder. Release v2 uses the same lock name, so the two
// releases exclude each other.
const (
	File     = "state.json"
	LockFile = "mutation.lock"
)

// State is the schema-2 document. Target is "<os>/<arch>:<home>". Source is
// the hash of the installer executable that completed the last run. System
// maps a provider (apt, brew, cask or windows) to the names of the items that
// setup installed. Pending holds "<resource>:<step>" marks of steps that can
// continue without setup. Linkage holds the resources of the Homebrew formulae
// that setup changed while their dependents have not passed a linkage check
// since. A Targets key is a cleaned absolute path, or that path, "#" and a
// slot.
type State struct {
	Schema    int                 `json:"schema"`
	Target    string              `json:"target"`
	Source    string              `json:"source,omitempty"`
	Selected  []string            `json:"selected"`
	Keep      []string            `json:"keep"`
	System    map[string][]string `json:"system"`
	Pending   []string            `json:"pending,omitempty"`
	Linkage   []string            `json:"linkage,omitempty"`
	V2Restore string              `json:"v2_restore,omitempty"`
	Targets   map[string]Record   `json:"targets"`
}

// Record covers one managed unit. Applied is the recorded fingerprint or
// value. Next is the intended one while a change is unfinished. Release is
// true while a removal is unfinished. Before is the first baseline: "absent",
// "value:<base64>", "backup:<path>" or "fp:<fingerprint>". Made lists the
// parent folders that setup created, nearest first. Before without Applied
// gives no ownership.
type Record struct {
	Resource string   `json:"resource"`
	Kind     string   `json:"kind"`
	Applied  string   `json:"applied,omitempty"`
	Next     string   `json:"next,omitempty"`
	Release  bool     `json:"release,omitempty"`
	Before   string   `json:"before,omitempty"`
	Made     []string `json:"made,omitempty"`
}

// ErrSchema1 reports a state of release v2. The command routes it to the
// import.
var ErrSchema1 = errors.New("schema 1 state: run the import")

// Store is the open state of a mutating run. It holds the installer lock
// until Close.
type Store struct {
	path, target string
	current      State
	release      func() error
}

// Open takes the installer lock in dir and reads the state. A missing file
// gives an empty state with Schema 0, and the first Update saves it as schema
// 2. A second Open of the same folder fails while the first is open.
func Open(dir, target string) (*Store, error) {
	release, err := platform.Lock(filepath.Join(dir, LockFile))
	if err != nil {
		return nil, err
	}
	current, err := Peek(dir, target)
	if err != nil {
		return nil, errors.Join(err, release())
	}
	return &Store{path: filepath.Join(dir, File), target: target, current: current, release: release}, nil
}

// Peek reads the state without the lock and writes nothing. The menu status
// and Check use it. A missing file or folder gives an empty state with Schema
// 0.
func Peek(dir, target string) (State, error) {
	empty := State{Target: target, Selected: []string{}, Keep: []string{}, System: map[string][]string{}, Targets: map[string]Record{}}
	data, err := platform.ReadDocument(filepath.Join(dir, File))
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, err
	}
	var header struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return empty, fmt.Errorf("invalid state; keep the file and examine it: %w", err)
	}
	if header.Schema == 1 {
		return empty, ErrSchema1
	}
	if header.Schema != 2 {
		return empty, fmt.Errorf("unsupported state schema %d; use the installer release that wrote it", header.Schema)
	}
	var loaded State
	if err := platform.Decode(data, &loaded); err != nil {
		return empty, fmt.Errorf("invalid state; keep the file and examine it: %w", err)
	}
	if loaded.Target != target {
		return empty, errors.New("state target differs from the active system, architecture or home; use the account and machine that wrote it")
	}
	loaded.normalize()
	if err := loaded.Validate(); err != nil {
		return empty, fmt.Errorf("invalid state; keep the file and examine it: %w", err)
	}
	return loaded, nil
}

// View returns a deep copy of the state.
func (s *Store) View() State { return s.current.Clone() }

// Update applies edit to a copy of the state, validates the copy and
// publishes it with an atomic rename. When edit, the validation or the
// publication fails, the file and the view do not change.
func (s *Store) Update(edit func(*State) error) error {
	next := s.current.Clone()
	if err := edit(&next); err != nil {
		return err
	}
	next.Schema, next.Target = 2, s.target
	next.normalize()
	if err := next.Validate(); err != nil {
		return fmt.Errorf("refused an invalid state change: %w", err)
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := platform.SaveDocument(s.path, append(data, '\n')); err != nil {
		return err
	}
	s.current = next
	return nil
}

// Close releases the installer lock.
func (s *Store) Close() error {
	if s.release == nil {
		return nil
	}
	release := s.release
	s.release = nil
	return release()
}

// Clone returns a deep copy.
func (s State) Clone() State {
	s.Selected = slices.Clone(s.Selected)
	s.Keep = slices.Clone(s.Keep)
	s.Pending = slices.Clone(s.Pending)
	s.Linkage = slices.Clone(s.Linkage)
	system := make(map[string][]string, len(s.System))
	for provider, names := range s.System {
		system[provider] = slices.Clone(names)
	}
	s.System = system
	targets := make(map[string]Record, len(s.Targets))
	for key, record := range s.Targets {
		record.Made = slices.Clone(record.Made)
		targets[key] = record
	}
	s.Targets = targets
	return s
}

func (s *State) normalize() {
	sortedUnique := func(values []string) []string {
		result := append([]string{}, values...)
		slices.Sort(result)
		return slices.Compact(result)
	}
	s.Selected, s.Keep = sortedUnique(s.Selected), sortedUnique(s.Keep)
	s.Pending = sortedUnique(s.Pending)
	if len(s.Pending) == 0 {
		s.Pending = nil
	}
	s.Linkage = sortedUnique(s.Linkage)
	if len(s.Linkage) == 0 {
		s.Linkage = nil
	}
	if s.System == nil {
		s.System = map[string][]string{}
	}
	for provider, names := range s.System {
		s.System[provider] = sortedUnique(names)
	}
	if s.Targets == nil {
		s.Targets = map[string]Record{}
	}
}

var resourceID = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,79}$`)

func plainText(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}

func cleanAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsFunc(path, unicode.IsControl)
}

func validKey(key string) bool {
	if cleanAbsolute(key) {
		return true
	}
	for i := len(key) - 1; i > 0; i-- {
		if key[i] == '#' && cleanAbsolute(key[:i]) && plainText(key[i+1:]) {
			return true
		}
	}
	return false
}

func validBefore(before string) bool {
	if before == "" || before == "absent" {
		return true
	}
	kind, value, ok := strings.Cut(before, ":")
	if !ok || value == "" {
		return false
	}
	switch kind {
	case "value":
		_, err := base64.StdEncoding.DecodeString(value)
		return err == nil
	case "backup", "fp":
		return !strings.ContainsFunc(value, unicode.IsControl)
	}
	return false
}

// Validate checks each field of a schema-2 state.
func (s State) Validate() error {
	if s.Schema != 2 {
		return fmt.Errorf("unsupported state schema %d", s.Schema)
	}
	if s.Target == "" {
		return errors.New("state has no target")
	}
	for _, id := range slices.Concat(s.Selected, s.Keep) {
		if !resourceID.MatchString(id) {
			return fmt.Errorf("invalid resource ID %q in the selection", id)
		}
	}
	for provider, names := range s.System {
		if provider != "apt" && provider != "brew" && provider != "cask" && provider != "windows" {
			return fmt.Errorf("invalid system provider %q", provider)
		}
		for _, name := range names {
			if !plainText(name) || strings.ContainsAny(name, " \t") {
				return fmt.Errorf("invalid %s item name %q", provider, name)
			}
		}
	}
	for _, mark := range s.Pending {
		resource, step, ok := strings.Cut(mark, ":")
		if !ok || !resourceID.MatchString(resource) || !plainText(step) {
			return fmt.Errorf("invalid pending mark %q", mark)
		}
	}
	for _, id := range s.Linkage {
		if !resourceID.MatchString(id) {
			return fmt.Errorf("invalid resource ID %q in the linkage checks", id)
		}
	}
	if s.V2Restore != "" && s.V2Restore != "switching" && s.V2Restore != "open" {
		return fmt.Errorf("invalid v2_restore value %q", s.V2Restore)
	}
	for _, key := range slices.Sorted(maps.Keys(s.Targets)) {
		record := s.Targets[key]
		if !validKey(key) {
			return fmt.Errorf("invalid target key %q", key)
		}
		if !resourceID.MatchString(record.Resource) {
			return fmt.Errorf("invalid resource ID %q for target %s", record.Resource, key)
		}
		if !slices.Contains([]string{"file", "tree", "link", "block", "field", "font"}, record.Kind) {
			return fmt.Errorf("invalid kind %q for target %s", record.Kind, key)
		}
		if !validBefore(record.Before) {
			return fmt.Errorf("invalid baseline for target %s", key)
		}
		if record.Applied == "" && record.Next == "" && record.Before == "" {
			return fmt.Errorf("target %s has an empty record", key)
		}
		if record.Release && (record.Applied == "" || record.Next == "") {
			return fmt.Errorf("target %s has a removal without an applied value or without the value to restore", key)
		}
		if len(record.Made) > platform.MaxCreatedDirectories || slices.ContainsFunc(record.Made, func(dir string) bool { return !cleanAbsolute(dir) }) {
			return fmt.Errorf("invalid created folders for target %s", key)
		}
	}
	return nil
}

// BeginTarget publishes the intended value of a target before its write. For
// a new target it stores r, which carries the first baseline and the created
// folders. For a recorded target it keeps the first baseline, the created
// folders and the applied value, and it ends an unfinished removal.
func (s *State) BeginTarget(key string, r Record) error {
	if r.Next == "" {
		return fmt.Errorf("target %s needs its intended value", key)
	}
	if s.Targets == nil {
		s.Targets = map[string]Record{}
	}
	if previous, ok := s.Targets[key]; ok {
		r.Applied = previous.Applied
		if previous.Before != "" {
			r.Before = previous.Before
		}
		if len(previous.Made) != 0 {
			r.Made = previous.Made
		}
	}
	r.Release = false
	r.Made = slices.Clone(r.Made)
	s.Targets[key] = r
	return nil
}

// Promote records a completed write: the intended value becomes the applied
// value.
func (s *State) Promote(key string) error {
	record, ok := s.Targets[key]
	if !ok || record.Next == "" || record.Release {
		return fmt.Errorf("target %s has no unfinished write", key)
	}
	record.Applied, record.Next = record.Next, ""
	s.Targets[key] = record
	return nil
}

// BeginRelease publishes an unfinished removal of an owned target. The value
// to restore is the first baseline, or absence when there is none.
func (s *State) BeginRelease(key string) error {
	record, ok := s.Targets[key]
	if !ok || record.Applied == "" {
		return fmt.Errorf("target %s has no applied value to release", key)
	}
	record.Release, record.Next = true, record.Before
	if record.Next == "" {
		record.Next = "absent"
	}
	s.Targets[key] = record
	return nil
}

// Drop deletes the record of a target. The restored baseline stays unowned.
func (s *State) Drop(key string) { delete(s.Targets, key) }

// Mark adds the pending mark "<resource>:<step>" before a step that can
// continue without setup.
func (s *State) Mark(step string) {
	if !slices.Contains(s.Pending, step) {
		s.Pending = append(s.Pending, step)
	}
}

// Clear removes a pending mark.
func (s *State) Clear(step string) {
	s.Pending = slices.DeleteFunc(s.Pending, func(mark string) bool { return mark == step })
}

// AddSystem records an item that setup installs through a system provider.
func (s *State) AddSystem(provider, name string) {
	if s.System == nil {
		s.System = map[string][]string{}
	}
	if !slices.Contains(s.System[provider], name) {
		s.System[provider] = append(s.System[provider], name)
	}
}

// RemoveSystem deletes the record of a system item. Setup calls it only
// after it verified that the item is absent.
func (s *State) RemoveSystem(provider, name string) {
	if s.System == nil {
		return
	}
	s.System[provider] = slices.DeleteFunc(s.System[provider], func(item string) bool { return item == name })
}

// HasSystem reports that provider has a record of name.
func (s State) HasSystem(provider, name string) bool {
	return slices.Contains(s.System[provider], name)
}
