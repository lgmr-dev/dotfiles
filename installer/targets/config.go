package targets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
)

// ConfigProvider is the engine.Provider of the resources with the action
// config. It resolves the configuration targets of the catalog to the known
// folders. On POSIX a target is a live link into the checkout. On Windows it
// is a copy, and a junction for a target with the mode junction. Each target
// is one unit of the Writer, so it keeps its first baseline and a removal
// restores it. Existing content without a record stays until the operator
// approves its replacement.
type ConfigProvider struct {
	Catalog  *catalog.Catalog
	Context  platform.Context
	Folders  platform.Folders
	Checkout string
	Writer   *Writer
}

const configSourceBlocked = "canonical configuration source is unavailable or has the wrong type; restore it before applying"

func (p *ConfigProvider) desired(r catalog.Resource) ([]Desired, string, error) {
	if p.Catalog == nil || p.Writer == nil || r.Action != "config" || !filepath.IsAbs(p.Checkout) {
		return nil, "", errors.New("configuration provider requires canonical source and state directories")
	}
	checkout, err := platform.ResolvePath(p.Checkout)
	if errors.Is(err, os.ErrNotExist) {
		checkout, err = platform.BindDestination(p.Checkout)
	}
	if err != nil {
		return nil, "", err
	}
	resolved, err := p.Catalog.TargetsFor(r.ID, p.Context, p.Folders, checkout)
	if err != nil {
		return nil, "", err
	}
	targets, blocked := []Desired{}, ""
	for _, item := range resolved {
		destination, err := platform.BindDestination(item.Destination)
		if err != nil {
			return nil, "", err
		}
		for _, pair := range [][2]string{{checkout, destination}, {destination, checkout}} {
			relative, err := filepath.Rel(pair[0], pair[1])
			if err == nil && (relative == "." || filepath.IsLocal(relative)) {
				return nil, "", errors.New("resolved configuration destination overlaps its source checkout")
			}
		}
		d := Desired{Key: destination, Resource: r.ID, Path: destination}
		info, statErr := os.Lstat(item.Source)
		valid := statErr == nil && (item.Directory && info.IsDir() || !item.Directory && info.Mode().IsRegular())
		switch {
		case item.Mode == "link" || item.Mode == "junction":
			d.Kind, d.Link = "link", item.Source
		case item.Directory:
			d.Kind, d.Tree = "tree", item.Source
		default:
			d.Kind, d.Tree = "file", item.Source
		}
		if !valid {
			blocked = configSourceBlocked
			if statErr != nil {
				blocked += ": " + statErr.Error()
			}
		}
		targets = append(targets, d)
	}
	return targets, blocked, nil
}

func (p *ConfigProvider) stale(rec engine.Records, r catalog.Resource, targets []Desired) []string {
	keys := []string{}
	for _, key := range slices.Sorted(maps.Keys(rec.State().Targets)) {
		if rec.State().Targets[key].Resource == r.ID && !slices.ContainsFunc(targets, func(d Desired) bool { return d.Key == key }) {
			keys = append(keys, key)
		}
	}
	return keys
}

func configToken(rows [][]string) string {
	encoded, _ := json.Marshal(rows)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// Inspect implements engine.Provider. It only reads. For a wanted resource
// it reports Satisfied when each target matches its record and the selected
// content, Changed when a target holds other content, and Missing or
// Unhealthy when a write is necessary. For a released resource it reports
// Absent without a record, Changed for a recorded target that holds other
// content, and Satisfied when the release can run. Preserved lists the
// changed targets. Token covers the observed fingerprint of each target.
func (p *ConfigProvider) Inspect(ctx context.Context, r catalog.Resource, want engine.Want, rec engine.Records) (engine.Finding, error) {
	if err := ctx.Err(); err != nil {
		return engine.Finding{}, err
	}
	if p.Writer == nil {
		return engine.Finding{}, errors.New("configuration provider requires canonical source and state directories")
	}
	st := rec.State()
	rows := [][]string{{fmt.Sprint(want.Present)}}
	unreadable, changed := []string{}, []string{}
	work, owned := 0, 0
	released := func(key string) error {
		live, err := p.Writer.InspectRecord(key, rec)
		if err != nil {
			return err
		}
		rows = append(rows, []string{key, live.State, live.Fingerprint, live.Want})
		switch {
		case live.State == StateUnreadable:
			unreadable = append(unreadable, live.Detail)
		case live.State == StateChanged && st.Targets[key].Applied != "":
			changed = append(changed, key)
		}
		work++
		return nil
	}
	if !want.Present {
		for _, key := range p.stale(rec, r, nil) {
			if err := released(key); err != nil {
				return engine.Finding{}, err
			}
		}
		finding := engine.Finding{Status: engine.Satisfied, Token: configToken(rows)}
		switch {
		case work == 0:
			finding.Status = engine.Absent
		case len(unreadable) != 0:
			finding.Status, finding.Reason = engine.Unknown, strings.Join(unreadable, "; ")
		case len(changed) != 0:
			finding.Status, finding.Preserved = engine.Changed, changed
			finding.Reason = "content changed after setup wrote it: " + strings.Join(changed, ", ")
		}
		return finding, nil
	}
	targets, blocked, err := p.desired(r)
	if err != nil {
		return engine.Finding{}, err
	}
	if blocked != "" {
		return engine.Finding{Status: engine.Blocked, Reason: blocked, Token: configToken(append(rows, []string{blocked}))}, nil
	}
	for _, d := range targets {
		live, err := p.Writer.Inspect(d, rec)
		if err != nil {
			return engine.Finding{}, err
		}
		rows = append(rows, []string{d.Key, d.Kind, live.State, live.Fingerprint, live.Want})
		record, has := st.Targets[d.Key]
		if has && record.Applied != "" {
			owned++
		}
		switch {
		case live.State == StateUnreadable:
			unreadable = append(unreadable, live.Detail)
		case live.State == StateChanged && live.Fingerprint != live.Want:
			changed = append(changed, d.Path)
		case live.State == StateApplied && record.Applied == live.Want && record.Kind == d.Kind && record.Next == "" && !record.Release:
		default:
			work++
		}
	}
	for _, key := range p.stale(rec, r, targets) {
		owned++
		if err := released(key); err != nil {
			return engine.Finding{}, err
		}
	}
	finding := engine.Finding{Status: engine.Satisfied, Token: configToken(rows)}
	switch {
	case len(unreadable) != 0:
		finding.Status, finding.Reason = engine.Unknown, strings.Join(unreadable, "; ")
	case len(changed) != 0:
		finding.Status, finding.Preserved = engine.Changed, changed
		finding.Reason = "existing content differs from the selected configuration: " + strings.Join(changed, ", ")
	case work != 0 && owned == 0:
		finding.Status = engine.Missing
	case work != 0:
		finding.Status, finding.Reason = engine.Unhealthy, "a configuration target differs from its record"
	}
	return finding, nil
}

func choiceOf(approved engine.Finding) Choice {
	if approved.Replace {
		return Replace
	}
	return Preserve
}

func configChange(err error) error {
	if errors.Is(err, ErrChanged) {
		return fmt.Errorf("configuration changed after approval: %w", err)
	}
	return err
}

// Apply implements engine.Provider. It releases each recorded target that
// the catalog no longer names, then converges each target. With
// approved.Replace it saves changed content first and replaces it.
func (p *ConfigProvider) Apply(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	targets, blocked, err := p.desired(r)
	if err != nil {
		return err
	}
	if blocked != "" {
		return errors.New(blocked)
	}
	choice := choiceOf(approved)
	for _, key := range p.stale(rec, r, targets) {
		if err := p.Writer.Release(ctx, key, rec, choice); err != nil {
			return configChange(err)
		}
	}
	for _, d := range targets {
		if err := p.Writer.Converge(ctx, d, rec, choice); err != nil {
			return configChange(err)
		}
	}
	return nil
}

// Remove implements engine.Provider. It releases each recorded target of the
// resource through the write order. It does not need the checkout.
func (p *ConfigProvider) Remove(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	if p.Writer == nil {
		return errors.New("configuration provider requires canonical source and state directories")
	}
	for _, key := range p.stale(rec, r, nil) {
		if err := p.Writer.Release(ctx, key, rec, choiceOf(approved)); err != nil {
			return configChange(err)
		}
	}
	return nil
}
