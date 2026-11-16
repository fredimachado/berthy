package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"berthy/internal/volarchive"
)

// The volume helper commands run in the throwaway containers that back up and restore a volume. They are not in the usage.
// The archive goes through stdout and stdin, so the only thing they say on stderr is one fixed line when they fail, or
// the counts of what they handled when they succeed: never a file name or a content.

// runVolumeArchive writes the tree at source to out as a volume archive.
func runVolumeArchive(source string, out, errout io.Writer) int {
	st, err := volarchive.Write(out, source)
	if err != nil {
		fmt.Fprintln(errout, "volume archive failed")
		return 1
	}
	return volumeSummary(st, errout)
}

// runVolumeExtract restores the archive in in into the empty directory target.
func runVolumeExtract(in io.Reader, target string, errout io.Writer) int {
	st, err := volarchive.Extract(in, target)
	if err != nil {
		fmt.Fprintln(errout, "volume extract failed")
		return 1
	}
	return volumeSummary(st, errout)
}

func volumeSummary(st volarchive.Stats, errout io.Writer) int {
	b, err := json.Marshal(st)
	if err != nil {
		return 1
	}
	fmt.Fprintln(errout, string(b))
	return 0
}
