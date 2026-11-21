package fetch

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/platform"
)

// Transfer bounds one download across reconnections. Limit covers the whole
// download with each attempt. Window is the interval that measures the
// progress of an attempt. Minimum is the byte count that a resumable attempt
// must deliver in each window. Attempts is the number of consecutive attempts
// that can keep no new bytes. Backoff is the delay after the first such
// attempt, and it doubles after each one. Each attempt opens new connections,
// and a server that offers byte ranges continues from the kept bytes. The
// SHA-256 check of the caller is the only integrity check: a range never
// decides which bytes are correct.
type Transfer struct {
	Limit    time.Duration
	Window   time.Duration
	Minimum  int64
	Attempts int
	Backoff  time.Duration
}

// DefaultTransfer is the policy of a download without an explicit Transfer.
var DefaultTransfer = Transfer{
	Limit:    15 * time.Minute,
	Window:   30 * time.Second,
	Minimum:  4 << 20,
	Attempts: 5,
	Backoff:  time.Second,
}

var errTransferSlow = errors.New("archive transfer too slow")

type permanentError struct{ error }

func (e permanentError) Unwrap() error { return e.error }

type transferState struct {
	file      *os.File
	digest    hash.Hash
	display   io.Writer
	name      string
	progress  *platform.DownloadProgress
	kept      int64
	total     int64
	validator string
	ranges    bool
	unhonored bool
	remember  func(total int64, validator string, ranges bool) error

	attempts, resumed, restarted int
}

func (s *transferState) summary() string {
	return fmt.Sprintf("%d attempts, %d resumed, %d restarted", s.attempts, s.resumed, s.restarted)
}

func (s *transferState) showProgress(total int64) {
	if s.display != nil {
		s.progress = &platform.DownloadProgress{Output: s.display, Name: s.name, Total: total}
	}
}

func (s *transferState) restart() error {
	s.restarted++
	if err := s.file.Truncate(0); err != nil {
		return permanentError{err}
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return permanentError{err}
	}
	s.digest.Reset()
	s.kept, s.total, s.validator, s.ranges = 0, -1, "", false
	s.showProgress(-1)
	return nil
}

func (s *transferState) unhonoredResume() error {
	s.unhonored = true
	return s.restart()
}

func (s *transferState) Write(data []byte) (int, error) {
	n, err := s.file.Write(data)
	s.digest.Write(data[:n])
	s.kept += int64(n)
	if s.progress != nil {
		_, _ = s.progress.Write(data[:n])
	}
	if err != nil {
		return n, permanentError{err}
	}
	return n, nil
}

// Fetch writes the complete response body of rawURL to file and digest and
// returns its length. The caller verifies the size and the SHA-256. A nil
// display shows no progress.
func (p Transfer) Fetch(ctx context.Context, client *http.Client, rawURL string, file *os.File, digest hash.Hash, display io.Writer, name string) (int64, error) {
	return p.fetch(ctx, client, rawURL, &transferState{file: file, digest: digest, display: display, name: name, total: -1})
}

func (p Transfer) fetch(ctx context.Context, client *http.Client, rawURL string, s *transferState) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, p.Limit)
	defer cancel()
	s.showProgress(s.total)
	name := s.name
	failures := 0
	for {
		start := s.kept
		err := p.attempt(ctx, client, rawURL, s)
		if err == nil {
			return s.kept, nil
		}
		if ctx.Err() != nil {
			return s.kept, fmt.Errorf("download %s stopped with %s received (%s): %w (last attempt: %v)", name, platform.FormatBytes(s.kept), s.summary(), ctx.Err(), err)
		}
		if permanent := (permanentError{}); errors.As(err, &permanent) {
			return s.kept, permanent.error
		}
		if s.ranges && s.kept > start {
			failures = 0
		} else {
			failures++
		}
		if failures >= p.Attempts {
			return s.kept, fmt.Errorf("download %s failed %d times without progress (%s): %w", name, failures, s.summary(), err)
		}
		if s.display != nil {
			action := "restarting"
			if s.ranges && s.kept > 0 {
				action = "resuming at " + platform.FormatBytes(s.kept)
			}
			_, _ = fmt.Fprintf(s.display, "\rDownloading %s: %v; reconnecting and %s\n", name, err, action)
		}
		if failures > 0 {
			delay := min(p.Backoff<<(failures-1), 30*time.Second)
			select {
			case <-ctx.Done():
				return s.kept, fmt.Errorf("download %s stopped with %s received (%s): %w (last attempt: %v)", name, platform.FormatBytes(s.kept), s.summary(), ctx.Err(), err)
			case <-time.After(delay):
			}
		}
	}
}

func (p Transfer) attempt(ctx context.Context, client *http.Client, rawURL string, s *transferState) (result error) {
	s.attempts++
	if s.kept > 0 && !s.ranges {
		if err := s.restart(); err != nil {
			return err
		}
	}
	attemptCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	c := *client
	if base, ok := transportOrDefault(client.Transport).(*http.Transport); ok {
		fresh := base.Clone()
		defer fresh.CloseIdleConnections()
		c.Transport = fresh
	}
	var received atomic.Int64
	var resumable atomic.Bool
	resumable.Store(s.ranges)
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(p.Window)
		defer ticker.Stop()
		var last int64
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				now := received.Load()
				if now == last || resumable.Load() && now-last < p.Minimum {
					stop(fmt.Errorf("%w: %s in %s", errTransferSlow, platform.FormatBytes(now-last), p.Window))
					return
				}
				last = now
			}
		}
	}()
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return permanentError{err}
	}
	if s.kept > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", s.kept))
		if s.validator != "" {
			req.Header.Set("If-Range", s.validator)
		}
	}
	response, err := c.Do(req)
	if err != nil {
		return attemptError(attemptCtx, err)
	}
	defer func() { result = errors.Join(result, response.Body.Close()) }()
	switch code := response.StatusCode; {
	case s.kept > 0 && code == http.StatusPartialContent:
		if !matchesContentRange(response.Header.Get("Content-Range"), s.kept, s.total) {
			if err := s.unhonoredResume(); err != nil {
				return err
			}
			return fmt.Errorf("archive server answered a resume with range %q", response.Header.Get("Content-Range"))
		}
		s.resumed++
	case code == http.StatusOK:
		if s.kept > 0 {
			if err := s.unhonoredResume(); err != nil {
				return err
			}
		}
		if response.ContentLength > catalog.MaxArchiveBytes {
			return permanentError{fmt.Errorf("archive download rejected: HTTP %d, length %d", code, response.ContentLength)}
		}
		s.total = response.ContentLength
		s.ranges = !s.unhonored && s.total > 0 && !response.Uncompressed && acceptsByteRanges(response.Header)
		s.validator = rangeValidator(response.Header)
		s.showProgress(s.total)
		resumable.Store(s.ranges)
		if s.remember != nil {
			if err := s.remember(s.total, s.validator, s.ranges); err != nil {
				return permanentError{err}
			}
		}
	case s.kept > 0 && code == http.StatusRequestedRangeNotSatisfiable:
		if err := s.unhonoredResume(); err != nil {
			return err
		}
		return fmt.Errorf("archive server refused to resume: HTTP %d", code)
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500:
		return fmt.Errorf("archive download: HTTP %d", code)
	default:
		return permanentError{fmt.Errorf("archive download rejected: HTTP %d, length %d", code, response.ContentLength)}
	}
	counted := io.TeeReader(response.Body, byteCounter{&received})
	_, err = io.Copy(s, io.LimitReader(counted, catalog.MaxArchiveBytes+1-s.kept))
	if s.kept > catalog.MaxArchiveBytes || s.total > 0 && s.kept > s.total {
		return permanentError{errors.New("archive size or SHA-256 differs from the reviewed pin")}
	}
	if err != nil {
		return attemptError(attemptCtx, err)
	}
	if s.total > 0 && s.kept < s.total {
		return fmt.Errorf("archive body ended after %s of %s", platform.FormatBytes(s.kept), platform.FormatBytes(s.total))
	}
	return nil
}

func attemptError(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); errors.Is(cause, errTransferSlow) {
		return cause
	}
	return err
}

type byteCounter struct{ count *atomic.Int64 }

func (c byteCounter) Write(data []byte) (int, error) {
	c.count.Add(int64(len(data)))
	return len(data), nil
}

func transportOrDefault(transport http.RoundTripper) http.RoundTripper {
	if transport == nil {
		return http.DefaultTransport
	}
	return transport
}

func acceptsByteRanges(header http.Header) bool {
	for _, value := range header.Values("Accept-Ranges") {
		for _, unit := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(unit), "bytes") {
				return true
			}
		}
	}
	return false
}

func rangeValidator(header http.Header) string {
	if tag := header.Get("ETag"); strings.HasPrefix(tag, `"`) {
		return tag
	}
	return header.Get("Last-Modified")
}

func matchesContentRange(value string, start, total int64) bool {
	spec, ok := strings.CutPrefix(value, "bytes ")
	if !ok || total <= start {
		return false
	}
	span, size, ok := strings.Cut(spec, "/")
	if !ok {
		return false
	}
	first, last, ok := strings.Cut(span, "-")
	if !ok {
		return false
	}
	parsed := make([]int64, 0, 3)
	for _, field := range []string{first, last, size} {
		number, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			return false
		}
		parsed = append(parsed, number)
	}
	return parsed[0] == start && parsed[1] == total-1 && parsed[2] == total
}
