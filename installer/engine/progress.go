package engine

import (
	"context"
	"fmt"
	"io"

	"github.com/luisgui1757/dotfiles/installer/platform"
)

// Progress displays approved work while it runs. Step receives the installer
// status and Output the live output of native commands. Both are for display
// only and must not fail. A preview carries no progress.
type Progress struct {
	Step   func(string)
	Output io.Writer
}

type progressKey struct{}

// WithProgress returns a context that carries the progress display. Its
// Output is also the display writer of the platform runner.
func WithProgress(ctx context.Context, progress Progress) context.Context {
	ctx = context.WithValue(ctx, progressKey{}, progress)
	if progress.Output != nil {
		ctx = platform.WithOutput(ctx, progress.Output)
	}
	return ctx
}

// ReportStep shows one status line of the run.
func ReportStep(ctx context.Context, format string, arguments ...any) {
	if progress, ok := ctx.Value(progressKey{}).(Progress); ok && progress.Step != nil {
		progress.Step(fmt.Sprintf(format, arguments...))
	}
}

// ReportDetail writes one line below the current step.
func ReportDetail(ctx context.Context, format string, arguments ...any) {
	platform.Detail(ctx, format, arguments...)
}

func progressVerb(action string) string {
	return map[string]string{"install": "Installing ", "update": "Updating ", "repair": "Repairing ", "remove": "Removing ", "forget": "Releasing the records of "}[action]
}
