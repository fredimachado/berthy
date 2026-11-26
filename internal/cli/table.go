package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/charmbracelet/x/ansi"
)

type cliTable struct{ rows [][]string }

func (t *cliTable) row(cells ...string) { t.rows = append(t.rows, cells) }
func (t *cliTable) write(w io.Writer, s cliStyle) error {
	if !s.color {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, row := range t.rows {
			if _, err := fmt.Fprintln(tw, strings.Join(row, "\t")); err != nil {
				return err
			}
		}
		return tw.Flush()
	}
	var widths []int
	for _, row := range t.rows {
		for i, cell := range row {
			for len(widths) <= i {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], ansi.StringWidth(cell))
		}
	}
	for _, row := range t.rows {
		for i, cell := range row {
			if _, err := io.WriteString(w, cell); err != nil {
				return err
			}
			if i < len(row)-1 {
				if _, err := io.WriteString(w, strings.Repeat(" ", widths[i]-ansi.StringWidth(cell)+2)); err != nil {
					return err
				}
			}
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}
	return nil
}
func (t *cliTable) header(s cliStyle, cells ...string) {
	for i := range cells {
		cells[i] = s.paint(toneBold, cells[i])
	}
	t.row(cells...)
}
