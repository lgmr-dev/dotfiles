package targets

import (
	"context"
	"fmt"

	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
)

// Capture saves the live unit of a record key outside the write order and
// returns its baseline reference. The import from release v2 calls it before
// the first change of a unit during the switch, so restore-v2 can give the
// unit back. It changes no state and no destination.
//
// The reference is Absent for a unit that does not exist, and made then
// lists what a write creates: the missing parent folders, nearest first, and
// before them the file of a part. For a unit that exists the reference is a
// copy in dir or an inline value, as Kind.Save gives it. A kind that cannot
// copy its unit gives "fp:" and the live fingerprint: such a unit can be
// compared and not given back.
func (w *Writer) Capture(ctx context.Context, key string, record state.Record, dir string) (saved string, made []string, err error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	d, kind, err := w.recorded(key, record)
	if err != nil {
		return "", nil, err
	}
	if err := bound(d.Path); err != nil {
		return "", nil, err
	}
	fingerprint, err := kind.Observe(d)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", d.Path, err)
	}
	if fingerprint == Absent {
		made, err = created(d)
		return Absent, made, err
	}
	if err := platform.PrepareDirectory(dir); err != nil {
		return "", nil, err
	}
	saved, err = kind.Save(ctx, d, dir)
	if err != nil {
		return "fp:" + fingerprint, nil, nil
	}
	return saved, nil, nil
}
