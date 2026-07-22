package main

import (
	"fmt"
	"io"
	"os"

	"github.com/alecthomas/kong"
	"github.com/zendext/scrape-fs/internal/buildinfo"
	"github.com/zendext/scrape-fs/internal/config"
)

type cli struct {
	Config  string           `help:"Path to the TOML configuration file." default:"${default_config}"`
	Version kong.VersionFlag `help:"Print version and exit."`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithLoader(args, stdout, stderr, config.Load)
}

type configLoader func(string) (config.Config, error)

func runWithLoader(args []string, stdout, stderr io.Writer, load configLoader) int {
	commandLine := cli{}
	requestedExitCode := -1
	parser, err := kong.New(
		&commandLine,
		kong.Name("scrapefsd"),
		kong.Description("ScrapeFS daemon."),
		kong.Vars{
			"default_config": config.DefaultPath,
			"version":        "scrapefsd " + buildinfo.Version,
		},
		kong.Writers(stdout, stderr),
		kong.Exit(func(code int) {
			requestedExitCode = code
		}),
	)
	if err != nil {
		fmt.Fprintf(stderr, "scrapefsd: %v\n", err)
		return 1
	}

	if _, err := parser.Parse(args); requestedExitCode >= 0 {
		return requestedExitCode
	} else if err != nil {
		fmt.Fprintf(stderr, "scrapefsd: %v\n", err)
		return 2
	}

	if _, err := load(commandLine.Config); err != nil {
		fmt.Fprintf(stderr, "scrapefsd: %v\n", err)
		return 1
	}
	return 0
}
