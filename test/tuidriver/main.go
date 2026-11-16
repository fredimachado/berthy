//go:build unix

// Command tuidriver runs a terminal program in a pseudo-terminal, drives it with keys from a script and takes snapshots of what
// its screen shows, as plain text and as HTML with the colours. It is how Berthy's terminal interface is checked.
//
//	tuidriver --command "berthy ui -e prod --config cfg.json" --size 120x40 --script steps.json --out DIR
//
// The script is a JSON list of steps: wait_text, wait_gone, keys, type, resize, sleep, snapshot and expect_exit. The values of
// the BERTHY_*TOKEN* variables and of the variables named with --redact-env never reach a file, and a run fails when one of them
// is on the screen.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// list is a flag that can be given more than once.
type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

func main() {
	var c config
	var redact, env list
	flag.StringVar(&c.command, "command", "", "the command line to run in the terminal")
	flag.StringVar(&c.size, "size", "120x40", "terminal size, COLSxROWS")
	flag.StringVar(&c.script, "script", "", "the JSON file of steps")
	flag.StringVar(&c.out, "out", "", "the directory for the snapshots and the results")
	flag.DurationVar(&c.timeout, "timeout", time.Minute, "the limit for the whole script")
	flag.Var(&redact, "redact-env", "the name of an environment variable whose value is never written (repeatable)")
	flag.Var(&env, "env", "NAME=VALUE to set for the program (repeatable)")
	flag.Parse()
	if c.command == "" || c.script == "" || c.out == "" || flag.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: tuidriver --command CMD --script steps.json --out DIR [--size COLSxROWS] [--timeout 60s] [--redact-env NAME]... [--env NAME=VALUE]...")
		os.Exit(2)
	}
	c.redactEnv, c.env = redact, env
	os.Exit(run(c, os.Stderr))
}
