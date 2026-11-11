package service

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"slices"
)

// CLI is the command line every program shares:
//
//	PROGRAM run|check -config FILE
//	PROGRAM version
//
// and the program's own commands that take -config too (the hub's codes).
type CLI struct {
	Program string // "safehome-hub"
	Version string
	Usage   string   // printed on a wrong command line
	Extra   []string // commands besides run and check
}

// Command is one parsed command line.
type Command struct {
	Name   string   // run, check or one of CLI.Extra
	Config string   // the -config file
	Args   []string // the arguments after the flags
}

// Parse reads the command line. When ok is false, the program has nothing more to
// do and exits with exit: it printed its version, or what was wrong with the line.
func (c CLI) Parse(args []string, stdout, stderr io.Writer) (cmd Command, exit int, ok bool) {
	if len(args) == 0 {
		fmt.Fprint(stderr, c.Usage)
		return Command{}, 2, false
	}
	cmd.Name = args[0]
	switch {
	case cmd.Name == "version":
		fmt.Fprintln(stdout, c.Program, c.Version)
		return Command{}, 0, false
	case cmd.Name != "run" && cmd.Name != "check" && !slices.Contains(c.Extra, cmd.Name):
		fmt.Fprintf(stderr, "unknown command %q\n%s", cmd.Name, c.Usage)
		return Command{}, 2, false
	}
	fs := flag.NewFlagSet(cmd.Name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cmd.Config, "config", "", "configuration file (required)")
	if err := fs.Parse(args[1:]); err != nil {
		return Command{}, 2, false
	}
	if cmd.Config == "" {
		fmt.Fprintf(stderr, "%s: -config is required\n", cmd.Name)
		return Command{}, 2, false
	}
	cmd.Args = fs.Args()
	return cmd, 0, true
}

// Logger is the programs' logger: text lines on w (the journal, under systemd).
func Logger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}
