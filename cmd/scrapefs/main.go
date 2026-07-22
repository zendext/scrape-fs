package main

import (
	"fmt"
	"io"
	"os"

	"github.com/alecthomas/kong"
	"github.com/zendext/scrape-fs/internal/buildinfo"
)

type cli struct {
	Version kong.VersionFlag `help:"Print version and exit."`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	commandLine := cli{}
	requestedExitCode := -1
	parser, err := kong.New(
		&commandLine,
		kong.Name("scrapefs"),
		kong.Description("ScrapeFS control CLI."),
		kong.Vars{"version": "scrapefs " + buildinfo.Version},
		kong.Writers(stdout, stderr),
		kong.Exit(func(code int) {
			requestedExitCode = code
		}),
	)
	if err != nil {
		fmt.Fprintf(stderr, "scrapefs: %v\n", err)
		return 1
	}

	context, err := parser.Parse(args)
	if requestedExitCode >= 0 {
		return requestedExitCode
	}
	if err != nil {
		fmt.Fprintf(stderr, "scrapefs: %v\n", err)
		return 2
	}
	if err := context.PrintUsage(false); err != nil {
		fmt.Fprintf(stderr, "scrapefs: %v\n", err)
		return 1
	}
	return 0
}
