package main

import (
	"flag"
	"io"
	"os"

	"github.com/zendext/scrape-fs/internal/buildinfo"
	"github.com/zendext/scrape-fs/internal/command"
	"github.com/zendext/scrape-fs/internal/config"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithLoader(args, stdout, stderr, config.Load)
}

type configLoader func(string) (config.Config, error)

func runWithLoader(args []string, stdout, stderr io.Writer, load configLoader) int {
	configPath := config.DefaultPath
	program := command.Program{
		Name:        "scrapefsd",
		Description: "ScrapeFS daemon.",
		Version:     buildinfo.Version,
		ConfigureFlags: func(flags *flag.FlagSet) {
			flags.StringVar(&configPath, "config", config.DefaultPath, "path to the TOML configuration file")
		},
		Execute: func() error {
			_, err := load(configPath)
			return err
		},
	}
	return program.Run(args, stdout, stderr)
}
