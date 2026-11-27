package main

import (
	"berthy/internal/agentclient"
	"berthy/internal/cli"
	"berthy/internal/credstore"
	"berthy/internal/manifest"
	"berthy/internal/uptimeprobe"
	"fmt"
	"os"
	// The binary carries the time zone database, so a manifest's timezone does not depend on what the server has installed.
	_ "time/tzdata"
)

func main() {
	if len(os.Args) >= 2 && os.Args[0] == manifest.UptimeProbeTarget && os.Args[1] == "probe" {
		os.Exit(uptimeprobe.Run(os.Args[2:], os.Stderr))
	}
	store, err := credstore.FromEnvironment(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, `BERTHY_TOKEN_STORE must be "none" or unset`)
		os.Exit(2)
	}
	agentclient.TokenStore = store
	cli.KeyHome = os.UserHomeDir
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
