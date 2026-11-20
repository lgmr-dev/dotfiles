// Package v2import moves an installation of release v2 to this release in one
// approved run, and gives it back with restore-v2. It reads the schema-1
// state and the baselines of release v2 with copies of the readers of commit
// fb15ce5 (v2.3.0), maps them to a schema-2 state in memory, builds the
// private tools beside the payloads of release v2 and then switches the
// targets through the engine. Release v3.1.0 deletes this package.
package v2import

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/luisgui1757/dotfiles/installer/platform"
)

// SourceCommit and SourceRelease name the last release v2: the import reads
// only a state that this release wrote, and restore-v2 hands the installation
// back to it.
const (
	SourceCommit  = "fb15ce5"
	SourceRelease = "v2.3.0"
)

// Remedy is the action for a state that the import cannot read or that holds
// an unfinished operation.
const Remedy = "check out " + SourceCommit + " (" + SourceRelease + "), run its setup and finish or abandon the interrupted operation"

// ErrRefused marks a schema-1 state that stops the import before any write.
var ErrRefused = errors.New("the import from release v2 cannot start")

func refused(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s; %s. Nothing was changed", ErrRefused, fmt.Sprintf(format, arguments...), Remedy)
}

var (
	resourceID  = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,79}$`)
	operationID = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

var retiredCapabilities = map[string]string{"sentinel-skills": "sentinel"}

type retiredResource struct {
	OS       string
	Retained bool
}

var retiredResources = map[string]retiredResource{
	"config.git":         {},
	"integration.vscode": {},
	"tool.flatpak":       {Retained: true},
	"tool.obsidian":      {OS: "linux"},
	"tool.zen":           {OS: "darwin"},
	"desktop.zen":        {OS: "darwin"},
}

func retiredSentinel(id string) bool {
	return slices.Contains([]string{"integration.sentinel", "skills.sentinel-agents", "skills.sentinel-claude", "skills.sentinel-kiro", "skills.sentinel-copilot"}, id)
}

// ResourceResume is the saved resume token of an observation of release v2.
type ResourceResume struct {
	Operation string `json:"operation"`
	Token     string `json:"token"`
}

// Observation is the observation of one resource in a receipt of release v2.
type Observation struct {
	Unknown                bool            `json:"unknown,omitempty"`
	Present                bool            `json:"present"`
	Healthy                bool            `json:"healthy"`
	HealthIssue            string          `json:"health_issue,omitempty"`
	Provider               string          `json:"provider,omitempty"`
	Identity               string          `json:"identity,omitempty"`
	Version                string          `json:"version,omitempty"`
	Fingerprint            string          `json:"fingerprint,omitempty"`
	Desired                string          `json:"desired,omitempty"`
	ApplyBlocked           string          `json:"apply_blocked,omitempty"`
	ApplyDetail            string          `json:"apply_detail,omitempty"`
	RemovalDetail          string          `json:"removal_detail,omitempty"`
	Preserved              []string        `json:"preserved,omitempty"`
	UnverifiedApplications []string        `json:"unverified_applications,omitempty"`
	Notes                  []string        `json:"notes,omitempty"`
	Consumers              []string        `json:"consumers,omitempty"`
	Pending                string          `json:"pending,omitempty"`
	Scope                  string          `json:"scope,omitempty"`
	Privileged             bool            `json:"privileged,omitempty"`
	Adoptable              bool            `json:"adoptable,omitempty"`
	CompletedOperation     string          `json:"completed_operation,omitempty"`
	RemovalDeferred        bool            `json:"removal_deferred,omitempty"`
	RestoredOperation      string          `json:"restored_operation,omitempty"`
	Restoring              bool            `json:"restoring,omitempty"`
	Inventory              string          `json:"inventory,omitempty"`
	ResourceResume         *ResourceResume `json:"resource_resume,omitempty"`
}

// Receipt is the record of one resource in a state of release v2.
type Receipt struct {
	Ownership   string      `json:"ownership"`
	Before      Observation `json:"before"`
	After       Observation `json:"after"`
	Status      string      `json:"status"`
	Reason      string      `json:"reason,omitempty"`
	Recovery    string      `json:"recovery,omitempty"`
	OperationID string      `json:"operation_id,omitempty"`
	Adopted     bool        `json:"adopted,omitempty"`
}

// Context is the system and architecture in a state of release v2.
type Context struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// State is the strict schema-1 state of release v2. Transaction holds an
// unfinished operation: the import reads no part of it and stops.
type State struct {
	Schema           int                `json:"schema"`
	Context          Context            `json:"context"`
	Target           string             `json:"target"`
	Source           string             `json:"source"`
	Generation       uint64             `json:"generation"`
	Selected         []string           `json:"selected"`
	Keep             []string           `json:"keep"`
	Receipts         map[string]Receipt `json:"receipts"`
	Transaction      json.RawMessage    `json:"transaction,omitempty"`
	PastTransactions []string           `json:"past_transactions,omitempty"`

	retired []string
}

func digest(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

func sameArtifact(a, b Observation) bool {
	return a.Unknown == b.Unknown && a.Present == b.Present && a.Provider == b.Provider && a.Identity == b.Identity && a.Fingerprint == b.Fingerprint && a.Version == b.Version && a.Scope == b.Scope && a.Privileged == b.Privileged
}

// LoadState reads the schema-1 state in a state folder with the validation
// of release v2. It returns the state and the exact bytes of the file. A
// pending transaction, an unreadable file and a rejected record give
// ErrRefused with the remedy. It writes nothing.
func LoadState(stateDir, home string, ctx platform.Context) (State, []byte, error) {
	var st State
	data, err := platform.ReadDocument(filepath.Join(stateDir, "state.json"))
	if err != nil {
		return st, nil, refused("cannot read the state of release v2: %v", err)
	}
	var header struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(data, &header); err != nil || header.Schema != 1 {
		return st, nil, refused("the state is not a schema-1 state of release v2")
	}
	if err := platform.Decode(data, &st); err != nil {
		return st, nil, refused("the state of release v2 is invalid: %v", err)
	}
	if st.Target != home || st.Context.OS != ctx.OS || st.Context.Arch != ctx.Arch {
		return st, nil, refused("the state of release v2 belongs to another home folder, system or architecture")
	}
	if transaction := bytes.TrimSpace(st.Transaction); len(transaction) != 0 && !bytes.Equal(transaction, []byte("null")) {
		return st, nil, refused("release v2 has an interrupted operation")
	}
	if st.Receipts == nil {
		st.Receipts = map[string]Receipt{}
	}
	selected := []string{}
	for _, id := range st.Selected {
		if successor := retiredCapabilities[id]; successor != "" {
			id = successor
		}
		if !resourceID.MatchString(id) {
			return st, nil, refused("the state of release v2 selects the invalid ID %q", id)
		}
		if !slices.Contains(selected, id) {
			selected = append(selected, id)
		}
	}
	st.Selected = selected
	for id, receipt := range st.Receipts {
		problem := ""
		switch {
		case !resourceID.MatchString(id):
			problem = "an invalid receipt ID"
		case receipt.Ownership != "created" && receipt.Ownership != "reused" && receipt.Ownership != "uncertain":
			problem = "an invalid ownership"
		case receipt.Recovery != "" && (!filepath.IsLocal(receipt.Recovery) || filepath.Clean(receipt.Recovery) != receipt.Recovery):
			problem = "an invalid recovery reference"
		case receipt.OperationID != "" && !operationID.MatchString(receipt.OperationID):
			problem = "an invalid operation identity"
		case receipt.Adopted && !receipt.Before.Present:
			problem = "an adopted resource without its original baseline"
		case receipt.Status == "in-progress":
			problem = "an operation that did not finish"
		}
		if problem != "" {
			return st, nil, refused("the state of release v2 has %s for %q", problem, id)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(st.Receipts)) {
		retired, ok := retiredResources[id]
		if !ok || retired.OS != "" && retired.OS != st.Context.OS {
			continue
		}
		if st.Receipts[id].Ownership != "reused" && !retired.Retained {
			return st, nil, refused("the state of release v2 records %s, which release v2.3.0 no longer reads", id)
		}
		delete(st.Receipts, id)
		st.retired = append(st.retired, id)
	}
	st.Keep = slices.DeleteFunc(slices.Clone(st.Keep), func(id string) bool {
		retired, ok := retiredResources[id]
		return ok && (retired.OS == "" || retired.OS == st.Context.OS)
	})
	for _, archive := range st.PastTransactions {
		if !filepath.IsLocal(archive) || filepath.Dir(archive) != "transactions" || len(filepath.Base(archive)) != 69 || filepath.Ext(archive) != ".json" {
			return st, nil, refused("the state of release v2 has an invalid transaction archive reference")
		}
	}
	return st, data, nil
}
