package command

import (
	"errors"
	"flag"
	"fmt"
	"io"
)

// Program defines the common behavior of a ScrapeFS command.
type Program struct {
	Name        string
	Description string
	Version     string
}

// Run executes a command and returns its process exit code.
func (p Program) Run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(p.Name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "print version and exit")
	flags.Usage = func() {
		fmt.Fprintf(stdout, "Usage: %s [options]\n\n", p.Name)
		fmt.Fprintln(stdout, p.Description)
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "Options:")
		fmt.Fprintln(stdout, "  --help     print help and exit")
		fmt.Fprintln(stdout, "  --version  print version and exit")
	}

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "%s: unexpected argument %q\n", p.Name, flags.Arg(0))
		return 2
	}

	if *showVersion {
		fmt.Fprintf(stdout, "%s %s\n", p.Name, p.Version)
		return 0
	}

	flags.Usage()
	return 0
}
