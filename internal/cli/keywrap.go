package cli

import (
	"berthy/internal/keywrap"
	"berthy/internal/state"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
)

func runKeyWrap(args []string, stdin io.Reader, out, errout io.Writer) int {
	if len(args) != 1 || !filepath.IsAbs(args[0]) || filepath.Clean(args[0]) != args[0] {
		fmt.Fprintln(errout, "key-wrap: expected an absolute clean key path")
		return 2
	}
	if runtime.GOOS != "linux" {
		fmt.Fprintln(errout, "key-wrap runs on the server")
		return 1
	}
	u, err := user.Lookup("berthy")
	if err != nil || u.Uid != strconv.Itoa(os.Geteuid()) {
		fmt.Fprintln(errout, "key-wrap: must run as the agent user")
		return 1
	}
	return keyWrap(args[0], stdin, out, errout)
}
func keyWrap(path string, stdin io.Reader, out, errout io.Writer) int {
	line, err := io.ReadAll(io.LimitReader(stdin, 1025))
	if err != nil {
		fmt.Fprintln(errout, "key-wrap: invalid wrapping key")
		return 1
	}
	if _, err = keywrap.Wrap(line, make([]byte, 32)); err != nil {
		fmt.Fprintln(errout, "key-wrap: invalid wrapping key")
		return 1
	}
	key, err := state.ReadSettledKey(path)
	defer clear(key)
	if errors.Is(err, state.ErrRotationUnsettled) {
		if _, err = io.WriteString(out, keywrap.UnsettledLine); err != nil {
			fmt.Fprintln(errout, "key-wrap: cannot write answer")
			return 1
		}
		return 3
	}
	if err != nil {
		fmt.Fprintln(errout, "key-wrap: cannot read the key")
		return 1
	}
	answer, err := keywrap.Wrap(line, key)
	if err != nil {
		fmt.Fprintln(errout, "key-wrap: cannot wrap the key")
		return 1
	}
	if _, err = out.Write(answer); err != nil {
		fmt.Fprintln(errout, "key-wrap: cannot write answer")
		return 1
	}
	return 0
}
