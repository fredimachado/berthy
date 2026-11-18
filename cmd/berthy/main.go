package main

import (
	"berthy/internal/cli"
	"os"
	// The binary carries the time zone database, so a manifest's timezone does not depend on what the server has installed.
	_ "time/tzdata"
)

func main() { os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr)) }
