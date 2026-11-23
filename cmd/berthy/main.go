package main

import (
	"berthy/internal/agentclient"
	"berthy/internal/cli"
	"berthy/internal/credstore"
	"fmt"
	"os"
	// The binary carries the time zone database, so a manifest's timezone does not depend on what the server has installed.
	_ "time/tzdata"
)

func main() {
	store, err := credstore.FromEnvironment(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, `BERTHY_TOKEN_STORE must be "none" or unset`)
		os.Exit(2)
	}
	agentclient.TokenStore = store
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
