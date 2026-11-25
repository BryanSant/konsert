package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/BryanSant/konsert/internal/elevate"
	"github.com/BryanSant/konsert/internal/install"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// exitCodeError ends konsertd with an elevated child's exit code; the child already
// printed its error, relayed to this terminal.
type exitCodeError struct{ code int }

func (e exitCodeError) Error() string { return fmt.Sprintf("exit code %d", e.code) }

// elevatedRun is set inside the elevated child (`konsertd elevated-child <pipe>`): it
// never elevates again, and install uses the password the parent handed over the pipe.
var elevatedRun struct {
	child    bool
	password string
}

// newElevatedChildCmd is the hidden command a UAC-elevated konsertd runs: it opens the
// parent's pipe, reads the command line (and password) from it, runs that command with
// its output relayed back, and reports the exit code.
func newElevatedChildCmd() *cobra.Command {
	return &cobra.Command{
		Use:    elevate.ChildCommand + " <pipe>",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runElevatedChild(args[0])
		},
	}
}

func runElevatedChild(pipeName string) error {
	conn, err := elevate.OpenChildPipe(pipeName)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	req, child, err := elevate.Accept(conn)
	if err != nil {
		return err
	}
	// Only the commands that elevate may be run this way.
	if req.Args[0] != "install" && req.Args[0] != "uninstall" {
		fmt.Fprintf(child.Stderr(), "konsertd: %q cannot be run elevated\n", req.Args[0])
		_ = child.Exit(2)
		return exitCodeError{2}
	}
	if req.Dir != "" {
		_ = os.Chdir(req.Dir)
	}
	elevatedRun.child = true
	elevatedRun.password = req.Password
	code := runRelayed(child, func() error {
		root := newRootCmd()
		root.SetArgs(append(req.Args, "--no-elevate"))
		root.SetIn(strings.NewReader(""))
		return root.Execute()
	})
	elevatedRun.password = ""
	if err := child.Exit(code); err != nil {
		return err
	}
	if code != 0 {
		return exitCodeError{code}
	}
	return nil
}

// runRelayed runs fn with os.Stdout and os.Stderr sent to the parent, and returns the
// exit code main would have used.
func runRelayed(child *elevate.Child, fn func() error) int {
	oldOut, oldErr := os.Stdout, os.Stderr
	var wg sync.WaitGroup
	relay := func(dst io.Writer) *os.File {
		r, w, err := os.Pipe()
		if err != nil {
			return nil
		}
		wg.Go(func() {
			_, _ = io.Copy(dst, r)
			_ = r.Close()
		})
		return w
	}
	outW, errW := relay(child.Stdout()), relay(child.Stderr())
	if outW != nil {
		os.Stdout = outW
	}
	if errW != nil {
		os.Stderr = errW
	}
	code := 0
	if err := fn(); err != nil {
		code = 1
		var ec exitCodeError
		if errors.As(err, &ec) {
			code = ec.code
		} else {
			fmt.Fprintf(os.Stderr, "konsertd: %v\n", err)
		}
	}
	os.Stdout, os.Stderr = oldOut, oldErr
	for _, w := range []*os.File{outW, errW} {
		if w != nil {
			_ = w.Close()
		}
	}
	wg.Wait()
	return code
}

// maybeElevate runs the command elevated when it needs administrator rights and this
// process lacks them: through UAC when someone at the desktop can approve it (the
// result relayed here), otherwise it fails with what to do instead. args is the
// command line for the elevated child (and for the message). handled reports that the
// command ran elevated or was refused; when false the caller goes on itself.
func maybeElevate(cmd *cobra.Command, op elevate.Op, noElevate bool, args []string, password func() (string, error)) (handled bool, err error) {
	if !install.ServiceSupported || elevatedRun.child {
		return false, nil
	}
	sit := elevate.Detect(noElevate)
	commandLine := elevate.CommandLine(append([]string{os.Args[0]}, args...))
	switch elevate.Decide(sit) {
	case elevate.RunHere:
		return false, nil
	case elevate.Refuse:
		return true, errors.New(elevate.Message(op, sit, commandLine))
	}
	req := elevate.Request{Args: args}
	req.Dir, _ = os.Getwd()
	if password != nil {
		// Asked here, before the UAC prompt, in the terminal the user is typing in.
		if req.Password, err = password(); err != nil {
			return true, err
		}
	}
	fmt.Fprintln(cmd.ErrOrStderr(), "Administrator rights needed: asking for approval (UAC)...")
	code, err := elevate.Run(req, cmd.OutOrStdout(), cmd.ErrOrStderr())
	req.Password = ""
	if errors.Is(err, elevate.ErrDeclined) {
		sit.Declined = true
		return true, errors.New(elevate.Message(op, sit, commandLine))
	}
	if err != nil {
		return true, err
	}
	if code != 0 {
		return true, exitCodeError{code}
	}
	return true, nil
}

// flagArgs renders the flags set on fs that cmdFlags also defines as --name=value
// arguments (slices repeated per element), skipping skip. It rebuilds an install
// command line from install or join (whose own arguments, such as a one-time join
// code, must not run twice).
func flagArgs(fs, cmdFlags *pflag.FlagSet, skip map[string]bool) []string {
	var out []string
	fs.Visit(func(f *pflag.Flag) {
		if skip[f.Name] || cmdFlags.Lookup(f.Name) == nil {
			return
		}
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			for _, v := range sv.GetSlice() {
				out = append(out, "--"+f.Name+"="+v)
			}
			return
		}
		if f.Value.Type() == "bool" && f.Value.String() == "true" {
			out = append(out, "--"+f.Name)
			return
		}
		out = append(out, "--"+f.Name+"="+f.Value.String())
	})
	return out
}
