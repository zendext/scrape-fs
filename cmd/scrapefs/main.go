package main

import (
	"io"
	"os"

	"github.com/zendext/scrape-fs/internal/buildinfo"
	"github.com/zendext/scrape-fs/internal/command"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	program := command.Program{
		Name:        "scrapefs",
		Description: "ScrapeFS control CLI.",
		Version:     buildinfo.Version,
	}
	return program.Run(args, stdout, stderr)
}
