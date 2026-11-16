//go:build !unix

// Command tuidriver needs a Unix pseudo-terminal; on other systems it only says so.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "tuidriver needs Linux, macOS or another Unix system")
	os.Exit(1)
}
