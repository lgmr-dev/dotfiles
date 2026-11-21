package platform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
)

// Command is one native command. Env adds to the environment of the process.
// Elevated declares that the command line already asks for administrator
// rights, for example through `sudo -n`. The runner adds no elevation.
// Interactive gives the child the terminal of the process: its input and
// output are not captured.
type Command struct {
	Path        string
	Args, Env   []string
	Dir         string
	Stdin       io.Reader
	Elevated    bool
	Interactive bool
}

// Runner runs a command in the foreground and returns its combined output.
// The output also goes to the display writer of the context, which the engine
// connects to the progress display and the apply log.
type Runner interface {
	Run(ctx context.Context, c Command) ([]byte, error)
}

// CommandError is a failed command. ExitCode is data: a vendor installer uses
// a code such as 3010 to ask for a restart.
type CommandError struct {
	Message  string
	ExitCode *int
}

func (err *CommandError) Error() string { return err.Message }

// ExitCode returns the exit code of the command that err describes.
func ExitCode(err error) (int, bool) {
	var native *CommandError
	if errors.As(err, &native) && native.ExitCode != nil {
		return *native.ExitCode, true
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), true
	}
	return 0, false
}

// Validate refuses a command that is not an absolute clean program path with
// bounded arguments and environment entries of the form KEY=value.
func (c Command) Validate() error {
	if !filepath.IsAbs(c.Path) || filepath.Clean(c.Path) != c.Path || strings.ContainsRune(c.Path, 0) || len(c.Args) > 256 || len(c.Env) > 128 {
		return errors.New("invalid native command boundary")
	}
	for _, argument := range c.Args {
		if len(argument) > 65536 || strings.ContainsRune(argument, 0) {
			return errors.New("invalid native command argument")
		}
	}
	for _, value := range c.Env {
		key, _, ok := strings.Cut(value, "=")
		if !ok || key == "" || len(value) > 65536 || strings.ContainsRune(value, 0) {
			return errors.New("invalid native command environment")
		}
	}
	return nil
}

const maxCommandOutput = 1 << 20

type boundedOutput struct {
	data     bytes.Buffer
	exceeded bool
}

func (output *boundedOutput) Write(data []byte) (int, error) {
	n := len(data)
	remaining := maxCommandOutput - output.data.Len()
	if n > remaining {
		data = data[:remaining]
		output.exceeded = true
	}
	_, err := output.data.Write(data)
	return n, err
}

// Exec is the Runner of the installer process. It shows the command line,
// streams the output to the display writer of the context and returns at most
// 1 MiB of it. A cancelled context does not stop a command that started,
// because a privileged child can continue after its parent stops.
type Exec struct{}

// Run implements Runner.
func (Exec) Run(ctx context.Context, c Command) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	Detail(ctx, "$ %s", DisplayCommand(c))
	command := exec.Command(c.Path, c.Args...)
	configureProcess(command, c.Interactive)
	command.Env = append(os.Environ(), c.Env...)
	command.Dir = c.Dir
	if c.Interactive {
		command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
		if c.Stdin != nil {
			command.Stdin = c.Stdin
		}
		return nil, commandFailure(command, command.Run(), nil)
	}
	command.Stdin = c.Stdin
	if c.Stdin == nil {
		command.Stdin = bytes.NewReader(nil)
	}
	var captured boundedOutput
	var combined io.Writer = &captured
	if display := Output(ctx); display != nil {
		combined = io.MultiWriter(&captured, display)
	}
	command.Stdout, command.Stderr = combined, combined
	err := command.Run()
	if captured.exceeded {
		err = errors.Join(err, errors.New("native command output exceeded 1 MiB; inspect the apply log"))
	}
	return captured.data.Bytes(), commandFailure(command, err, captured.data.Bytes())
}

func commandFailure(command *exec.Cmd, err error, output []byte) error {
	if err == nil {
		return nil
	}
	failure := &CommandError{Message: fmt.Sprintf("native command failed: %s%s", err.Error(), Diagnostic(output))}
	if command.ProcessState != nil {
		code := command.ProcessState.ExitCode()
		failure.ExitCode = &code
	}
	return failure
}

// Diagnostic returns the last 8 KiB of command output for an error message,
// without terminal control characters.
func Diagnostic(output []byte) string {
	prefix := "\n"
	if len(output) > 8192 {
		output = output[len(output)-8192:]
		prefix += "[last 8 KiB of captured output]\n"
	}
	text := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, strings.ToValidUTF8(string(output), "?"))
	if text = strings.TrimSpace(text); text == "" {
		return ""
	}
	return prefix + text
}
