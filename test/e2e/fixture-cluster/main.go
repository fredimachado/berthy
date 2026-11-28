// fixture-cluster inspects delivery checkpoints and seeds credentials in disposable e2e state.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"os"

	"berthy/internal/agent"
	"berthy/internal/api"
)

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("expected notified or secret")
	}
	switch args[0] {
	case "notified":
		if len(args) != 4 {
			return fmt.Errorf("expected notified STATE_DB CHECK STATE")
		}
		u := url.URL{Scheme: "file", Path: args[1], RawQuery: "mode=ro"}
		db, err := sql.Open("sqlite", u.String())
		if err != nil {
			return err
		}
		defer db.Close()
		var got string
		err = db.QueryRow("SELECT notified_state FROM checks WHERE environment=? AND name=?", agent.ClusterEnvironment, args[2]).Scan(&got)
		if err != nil {
			return err
		}
		if got != args[3] {
			return fmt.Errorf("check %s has notified state %q, want %q", args[2], got, args[3])
		}
		return nil
	case "secret":
		if len(args) != 3 {
			return fmt.Errorf("expected secret CONFIG NAME")
		}
		f, err := agent.LoadFile(args[1])
		if err != nil {
			return err
		}
		value, err := io.ReadAll(io.LimitReader(os.Stdin, api.MaxSecretValue+1))
		if err != nil || len(value) == 0 || len(value) > api.MaxSecretValue {
			return fmt.Errorf("invalid fixture credential input")
		}
		defer clear(value)
		return agent.WriteClusterSecret(context.Background(), f.Cluster, args[2], value)
	default:
		return fmt.Errorf("unknown fixture command")
	}
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
