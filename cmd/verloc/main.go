package main

import (
	"fmt"
	"os"

	"verloc/internal/server"
)

// Set via -ldflags at build time, e.g.:
//
//	-X main.version=1.2.3 -X main.commit=abc1234 -X main.buildTime=2026-08-25T12:00:00Z
var (
	version   = "dev"
	commit    = "none"
	buildTime = "unknown"
)

func main() {
	for _, a := range os.Args[1:] {
		if a == "-version" || a == "--version" || a == "version" {
			fmt.Printf("verloc %s (commit %s, built %s)\n", version, commit, buildTime)
			return
		}
	}
	server.Version = version
	server.Commit = commit
	server.Run()
}
