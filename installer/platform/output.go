package platform

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
)

type outputKey struct{}

// WithOutput returns a context that carries the display writer for live
// command output. The writer is for display only and must not fail.
func WithOutput(ctx context.Context, output io.Writer) context.Context {
	return context.WithValue(ctx, outputKey{}, output)
}

// Output returns the display writer of ctx, or nil. A preview carries none.
func Output(ctx context.Context) io.Writer {
	output, _ := ctx.Value(outputKey{}).(io.Writer)
	return output
}

// Detail writes one line to the display writer of ctx, when it has one.
func Detail(ctx context.Context, format string, arguments ...any) {
	if output := Output(ctx); output != nil {
		_, _ = fmt.Fprintf(output, format+"\n", arguments...)
	}
}

// DisplayCommand shows which program runs and its arguments, without its
// environment. It shortens a long line, because the display is not evidence.
func DisplayCommand(c Command) string {
	words := []string{path.Base(strings.ReplaceAll(c.Path, `\`, "/"))}
	for _, argument := range c.Args {
		if strings.ContainsAny(argument, " \t\n") {
			argument = fmt.Sprintf("%q", argument)
		}
		words = append(words, argument)
	}
	line := strings.Join(words, " ")
	if runes := []rune(line); len(runes) > 240 {
		line = string(runes[:240]) + "…"
	}
	return line
}

// FormatBytes prints a byte count in KiB or MiB.
func FormatBytes(count int64) string {
	if count < 1<<20 {
		return fmt.Sprintf("%.1f KiB", float64(count)/(1<<10))
	}
	return fmt.Sprintf("%.1f MiB", float64(count)/(1<<20))
}

// FormatElapsed prints a duration for the progress display.
func FormatElapsed(elapsed time.Duration) string {
	if elapsed < time.Minute {
		return elapsed.Round(100 * time.Millisecond).String()
	}
	return elapsed.Round(time.Second).String()
}

// DownloadProgress counts the bytes written to it and rewrites one display
// line at most two times each second. A Total of zero or less is unknown.
type DownloadProgress struct {
	Output io.Writer
	Name   string
	Total  int64
	done   int64
	shown  time.Time
}

// Write counts data and never fails.
func (p *DownloadProgress) Write(data []byte) (int, error) {
	p.done += int64(len(data))
	if now := time.Now(); now.Sub(p.shown) >= 500*time.Millisecond {
		p.shown = now
		if p.Total > 0 {
			_, _ = fmt.Fprintf(p.Output, "\rDownloading %s: %d%% of %s", p.Name, p.done*100/p.Total, FormatBytes(p.Total))
		} else {
			_, _ = fmt.Fprintf(p.Output, "\rDownloading %s: %s", p.Name, FormatBytes(p.done))
		}
	}
	return len(data), nil
}
