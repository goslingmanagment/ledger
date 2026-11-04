// Package cli holds the subcommands of the ledger binary. Each feature adds its commands
// from its own file with Command in init(), so parallel branches do not edit one list.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// Cmd is one subcommand. Name may contain a space for two-word commands ("export ledger-md").
type Cmd struct {
	Name    string
	Summary string
	Run     func(ctx context.Context, args []string) error
}

var commands = map[string]Cmd{}

// Command registers a subcommand.
func Command(c Cmd) {
	if _, dup := commands[c.Name]; dup {
		panic("duplicate command " + c.Name)
	}
	commands[c.Name] = c
}

// Exit is an error with a process exit code (2 data error, 3 checks failed, 4 no answer).
type Exit struct {
	Code int
	Msg  string
}

func (e Exit) Error() string { return e.Msg }

// Main dispatches args (without the program name) and returns the process exit code.
func Main(ctx context.Context, args []string, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(stderr)
		return 0
	}
	var cmd Cmd
	var rest []string
	if len(args) > 1 {
		if c, ok := commands[args[0]+" "+args[1]]; ok {
			cmd, rest = c, args[2:]
		}
	}
	if cmd.Run == nil {
		c, ok := commands[args[0]]
		if !ok {
			fmt.Fprintf(stderr, "ledger: неизвестная команда %q\n\n", strings.Join(args[:min(2, len(args))], " "))
			usage(stderr)
			return 4
		}
		cmd, rest = c, args[1:]
	}
	if err := cmd.Run(ctx, rest); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		code := 2
		if e, ok := err.(Exit); ok {
			code = e.Code
		}
		fmt.Fprintf(stderr, "ledger %s: %v\n", cmd.Name, err)
		return code
	}
	return 0
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "ledger — финансовая база агентства. Команды:")
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "  %-22s %s\n", n, commands[n].Summary)
	}
}

// Flags returns a FlagSet that prints its usage to stderr.
func Flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("ledger "+name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}
