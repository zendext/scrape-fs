package command

import (
	"errors"
	"flag"
	"fmt"
	"io"
)

// Program defines the common behavior of a ScrapeFS command.
type Program struct {
	Name           string
	Description    string
	Version        string
	ConfigureFlags func(*flag.FlagSet)
	Execute        func() error
}

// Run executes a command and returns its process exit code.
func (p Program) Run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(p.Name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	showHelp := flags.Bool("help", false, "print help and exit")
	showVersion := flags.Bool("version", false, "print version and exit")
	if p.ConfigureFlags != nil {
		p.ConfigureFlags(flags)
	}
	flags.Usage = func() {
		fmt.Fprintf(stdout, "Usage: %s [options]\n\n", p.Name)
		fmt.Fprintln(stdout, p.Description)
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "Options:")
		flags.SetOutput(stdout)
		flags.PrintDefaults()
		flags.SetOutput(stderr)
	}

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *showHelp {
		flags.Usage()
		return 0
	}

	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "%s: unexpected argument %q\n", p.Name, flags.Arg(0))
		return 2
	}

	if *showVersion {
		fmt.Fprintf(stdout, "%s %s\n", p.Name, p.Version)
		return 0
	}

	if p.Execute != nil {
		if err := p.Execute(); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", p.Name, err)
			return 1
		}
		return 0
	}

	flags.Usage()
	return 0
}
