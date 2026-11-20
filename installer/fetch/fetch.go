// Package fetch downloads the pinned archives with resume, verifies their
// SHA-256 and extracts them: tar, gzip, xz, ZIP, Debian packages and disk
// images.
package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/platform"
)

// MaxPackageBytes and MaxPackageEntries bound one extracted payload.
const (
	MaxPackageBytes   int64 = 2 << 30
	MaxPackageEntries       = 100000
)

// Options holds the process boundary of a download. A nil Client uses a
// default client. Cache is the folder that keeps an unfinished download for
// the next run; without it the download uses a temporary file beside the
// destination. DpkgDeb is the absolute path of dpkg-deb for the deb format.
// A zero Transfer uses DefaultTransfer.
type Options struct {
	Client   *http.Client
	Cache    string
	DpkgDeb  string
	Transfer Transfer
}

type cacheRecord struct {
	URL       string `json:"url"`
	Total     int64  `json:"total"`
	Validator string `json:"validator,omitempty"`
}

// Download gets the archive of pin, verifies the complete bytes against the
// SHA-256 of the pin and then extracts them to destination, which must not
// exist. It never runs a downloaded installer. With a cache, an interrupted
// download continues in the next run, and a verified download stays in the
// cache until its extraction succeeds.
func Download(ctx context.Context, pin catalog.ArchivePin, destination string, o Options) (result error) {
	if err := pin.Validate(); err != nil {
		return err
	}
	client := o.Client
	if client == nil {
		client = &http.Client{}
	}
	safe := *client
	safe.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || req.URL.Scheme != "https" || req.URL.User != nil {
			return errors.New("unsafe archive redirect")
		}
		if client.CheckRedirect != nil {
			return client.CheckRedirect(req, via)
		}
		return nil
	}
	source, err := url.Parse(pin.URL)
	if err != nil {
		return err
	}
	policy := o.Transfer
	if policy == (Transfer{}) {
		policy = DefaultTransfer
	}
	digest := sha256.New()
	name := path.Base(source.Path)
	state := &transferState{digest: digest, display: platform.Output(ctx), name: name, total: -1}
	complete := false
	var record string
	if o.Cache == "" {
		state.file, err = os.CreateTemp(filepath.Dir(destination), ".download-*")
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, state.file.Close(), os.Remove(state.file.Name())) }()
	} else {
		if err := platform.PrepareDirectory(o.Cache); err != nil {
			return err
		}
		record = filepath.Join(o.Cache, pin.SHA256+".json")
		state.file, err = os.OpenFile(filepath.Join(o.Cache, pin.SHA256+".part"), os.O_RDWR|os.O_CREATE, 0600)
		if err != nil {
			return err
		}
		discard := func() error {
			return errors.Join(ignoreMissing(os.Remove(state.file.Name())), ignoreMissing(os.Remove(record)))
		}
		defer func() {
			result = errors.Join(result, state.file.Close())
			if result == nil || errors.Is(result, errPinMismatch) {
				result = errors.Join(result, discard())
			}
		}()
		complete, err = resumeCache(state, record, pin)
		if err != nil {
			return err
		}
		state.remember = func(total int64, validator string, ranges bool) error {
			if !ranges {
				return ignoreMissing(os.Remove(record))
			}
			data, err := json.Marshal(cacheRecord{URL: pin.URL, Total: total, Validator: validator})
			if err != nil {
				return err
			}
			return platform.SaveDocument(record, data)
		}
	}
	started := time.Now()
	n := state.kept
	if !complete {
		n, err = policy.fetch(ctx, &safe, pin.URL, state)
		if err != nil {
			return err
		}
	}
	if n > catalog.MaxArchiveBytes || hex.EncodeToString(digest.Sum(nil)) != pin.SHA256 {
		return errPinMismatch
	}
	if state.display != nil {
		if complete {
			_, _ = fmt.Fprintf(state.display, "\rReused the verified download of %s (%s); SHA-256 matches the reviewed pin\n", name, platform.FormatBytes(n))
		} else {
			_, _ = fmt.Fprintf(state.display, "\rDownloaded %s (%s in %s); SHA-256 matches the reviewed pin\n", name, platform.FormatBytes(n), platform.FormatElapsed(time.Since(started)))
		}
	}
	if _, err := state.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return Extract(ctx, state.file, n, pin, destination, o.DpkgDeb)
}

var errPinMismatch = errors.New("archive size or SHA-256 differs from the reviewed pin")

func ignoreMissing(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func resumeCache(state *transferState, record string, pin catalog.ArchivePin) (complete bool, err error) {
	size, err := io.Copy(state.digest, io.LimitReader(state.file, catalog.MaxArchiveBytes+1))
	if err != nil {
		return false, err
	}
	if size == 0 {
		return false, nil
	}
	if size <= catalog.MaxArchiveBytes && hex.EncodeToString(state.digest.Sum(nil)) == pin.SHA256 {
		state.kept = size
		return true, nil
	}
	var saved cacheRecord
	data, err := platform.ReadDocument(record)
	if err == nil {
		err = platform.Decode(data, &saved)
	}
	if err != nil || saved.URL != pin.URL || saved.Total <= size || saved.Total > catalog.MaxArchiveBytes {
		if err := state.restart(); err != nil {
			return false, err
		}
		state.restarted = 0
		return false, nil
	}
	state.kept, state.total, state.validator, state.ranges = size, saved.Total, saved.Validator, true
	return false, nil
}
