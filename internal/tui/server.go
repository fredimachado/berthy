package tui

import (
	"context"
	"fmt"
	"image/color"
	"sort"
	"strings"
	"time"

	"berthy/internal/api"
	"berthy/internal/metrics"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// maxSamples is how many samples the server tab keeps: 20 minutes at one every 10 seconds.
const maxSamples = 120

// meter is an animated progress bar and the value it moves to.
type meter struct {
	bar    progress.Model
	target float64
}

// ramp is the colours of a usage bar from empty to full: green, then yellow, then red.
func ramp(st styles) []color.Color {
	return lipgloss.Blend1D(101, st.p.ok, st.p.warn, st.p.err)
}

func newMeter(st styles, colors []color.Color) *meter {
	pick := func(total, current float64) color.Color {
		return colors[min(max(int(current*100+0.5), 0), 100)]
	}
	bar := progress.New(
		progress.WithColorFunc(pick),
		progress.WithFillCharacters('█', '░'),
		progress.WithoutPercentage(),
		progress.WithSpringOptions(14, 1),
	)
	bar.EmptyColor = st.p.border
	return &meter{bar: bar}
}

// set moves the bar to p (0 to 1) and returns the command that animates it. With animation off the bar jumps.
func (m *meter) set(p float64) tea.Cmd {
	m.target = min(max(p, 0), 1)
	return m.bar.SetPercent(m.target)
}

// view draws the bar w cells wide.
func (m *meter) view(w int, animate bool) string {
	m.bar.SetWidth(max(w, 4))
	if animate {
		return m.bar.View()
	}
	return m.bar.ViewAs(m.target)
}

// serverModel is the server tab: the host's meters, the disks and the containers.
type serverModel struct {
	samples []metrics.Sample
	df      *dfInfo
	meters  map[string]*meter
	colors  []color.Color
	table   table.Model
	names   map[string]string

	gen    int
	cancel context.CancelFunc
	state  streamState
	text   string
	retry  time.Duration
	live   bool
}

func newServerModel(st styles) serverModel {
	tb := table.New(table.WithFocused(true), table.WithStyles(st.tableStyles()))
	return serverModel{
		meters: map[string]*meter{},
		colors: ramp(st),
		table:  tb,
		names:  map[string]string{},
	}
}

func (s *serverModel) meter(st styles, name string) *meter {
	if m, ok := s.meters[name]; ok {
		return m
	}
	m := newMeter(st, s.colors)
	s.meters[name] = m
	return m
}

// latest is the newest sample.
func (s *serverModel) latest() (metrics.Sample, bool) {
	if len(s.samples) == 0 {
		return metrics.Sample{}, false
	}
	return s.samples[len(s.samples)-1], true
}

// addSamples adds samples in time order, ignoring any that is not newer than the newest one, and moves the meters.
func (s *serverModel) addSamples(st styles, samples []metrics.Sample) []tea.Cmd {
	for _, sm := range samples {
		if n := len(s.samples); n > 0 && !sm.Time.After(s.samples[n-1].Time) {
			continue
		}
		s.samples = append(s.samples, sm)
	}
	if len(s.samples) > maxSamples {
		s.samples = append([]metrics.Sample(nil), s.samples[len(s.samples)-maxSamples:]...)
	}
	return s.moveMeters(st)
}

// seed replaces the history with the samples of the last minutes.
func (s *serverModel) seed(st styles, samples []metrics.Sample) []tea.Cmd {
	sort.SliceStable(samples, func(i, j int) bool { return samples[i].Time.Before(samples[j].Time) })
	s.samples = nil
	return s.addSamples(st, samples)
}

func fraction(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total)
}

// usage is the used share of a total with the room left over, for the memory and swap meters.
func memoryUsed(total, free *uint64) (used, tot uint64, ok bool) {
	if total == nil || free == nil || *total == 0 {
		return 0, 0, false
	}
	return *total - min(*free, *total), *total, true
}

func (s *serverModel) moveMeters(st styles) []tea.Cmd {
	sm, ok := s.latest()
	if !ok {
		return nil
	}
	var cmds []tea.Cmd
	add := func(c tea.Cmd) {
		if c != nil {
			cmds = append(cmds, c)
		}
	}
	h := sm.Host
	if h.CPUPercent != nil {
		add(s.meter(st, "cpu").set(*h.CPUPercent / 100))
	}
	if used, tot, ok := memoryUsed(h.MemoryTotal, h.MemoryAvailable); ok {
		add(s.meter(st, "mem").set(fraction(used, tot)))
	}
	if used, tot, ok := memoryUsed(h.SwapTotal, h.SwapFree); ok {
		add(s.meter(st, "swap").set(fraction(used, tot)))
	}
	for _, mt := range h.Mounts {
		if mt.Total > 0 {
			add(s.meter(st, "mount:"+mt.Path).set(fraction(mt.Total-min(mt.Available, mt.Total), mt.Total)))
		}
	}
	return cmds
}

// setDF takes the disk usage of the host and Docker; its mounts take the place of the sample's when the sample has none.
func (s *serverModel) setDF(st styles, df dfInfo) []tea.Cmd {
	s.df = &df
	var cmds []tea.Cmd
	for _, mt := range df.Host {
		if mt.Total > 0 {
			if c := s.meter(st, "mount:"+mt.Path).set(fraction(mt.Total-min(mt.Available, mt.Total), mt.Total)); c != nil {
				cmds = append(cmds, c)
			}
		}
	}
	return cmds
}

// mounts are the mounts to show: the newest sample's, or else those of df.
func (s *serverModel) mounts() []metrics.Mount {
	var all []metrics.Mount
	if sm, ok := s.latest(); ok && len(sm.Host.Mounts) > 0 {
		all = sm.Host.Mounts
	} else if s.df != nil {
		all = s.df.Host
	}
	return distinctMounts(all)
}

// distinctMounts keeps one mount of each file system: mounts that show the same sizes are the same disk, and the shortest path
// names it best.
func distinctMounts(all []metrics.Mount) []metrics.Mount {
	type size struct{ total, avail uint64 }
	best := map[size]int{}
	for i, m := range all {
		k := size{m.Total, m.Available}
		if j, ok := best[k]; !ok || len(m.Path) < len(all[j].Path) {
			best[k] = i
		}
	}
	var out []metrics.Mount
	for i, m := range all {
		if best[size{m.Total, m.Available}] == i {
			out = append(out, m)
		}
	}
	return out
}

// update moves the animations of the meters.
func (s *serverModel) updateFrame(msg progress.FrameMsg) []tea.Cmd {
	var cmds []tea.Cmd
	for _, m := range s.meters {
		var c tea.Cmd
		m.bar, c = m.bar.Update(msg)
		if c != nil {
			cmds = append(cmds, c)
		}
	}
	return cmds
}

// rates is the host's network and disk traffic per second, from the last two samples.
func (s *serverModel) rates() (rx, tx, rd, wr float64, ok bool) {
	n := len(s.samples)
	if n < 2 {
		return 0, 0, 0, 0, false
	}
	a, b := s.samples[n-2], s.samples[n-1]
	dt := b.Time.Sub(a.Time).Seconds()
	if dt <= 0 {
		return 0, 0, 0, 0, false
	}
	rate := func(x, y *uint64) float64 {
		if x == nil || y == nil || *y < *x {
			return 0
		}
		return float64(*y-*x) / dt
	}
	return rate(a.Host.NetworkRXBytes, b.Host.NetworkRXBytes), rate(a.Host.NetworkTXBytes, b.Host.NetworkTXBytes),
		rate(a.Host.DiskReadBytes, b.Host.DiskReadBytes), rate(a.Host.DiskWriteBytes, b.Host.DiskWriteBytes), true
}

// compactBytes is a byte count in at most six characters: "12.3M".
func compactBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	if v >= 100 {
		return fmt.Sprintf("%.0f%c", v, "KMGTP"[exp])
	}
	return fmt.Sprintf("%.1f%c", v, "KMGTP"[exp])
}

// label names a container by the app and role it belongs to.
func (s *serverModel) label(id string) (string, bool) {
	for known, name := range s.names {
		if strings.HasPrefix(id, known) || strings.HasPrefix(known, id) {
			return name, true
		}
	}
	return id[:min(len(id), 12)], false
}

// setNames maps the containers of the apps to names.
func (s *serverModel) setNames(apps []api.App) {
	names := map[string]string{}
	for _, a := range apps {
		for _, r := range a.Roles {
			for _, rep := range r.Replicas {
				if rep.ContainerID != "" {
					names[rep.ContainerID] = a.Ref.Name + "/" + r.Name
				}
			}
		}
	}
	s.names = names
}

// containerColumns lays the containers table out for w cells.
func containerColumns(w int) []table.Column {
	cols := []table.Column{{Title: "", Width: 1}, {Title: "Container", Width: 0}, {Title: "CPU", Width: 6}, {Title: "Memory", Width: 13}, {Title: "Net rx/tx", Width: 13}, {Title: "Block r/w", Width: 13}}
	used := 0
	for _, c := range cols {
		used += c.Width + 2
	}
	cols[1].Width = max(w-used-2, 10)
	return cols
}

// containerRows are the containers, busiest first.
func (s *serverModel) containerRows(st styles) []table.Row {
	sm, ok := s.latest()
	if !ok {
		return nil
	}
	cs := append([]metrics.Container(nil), sm.Containers...)
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := -1.0, -1.0
		if cs[i].CPUPercent != nil {
			a = *cs[i].CPUPercent
		}
		if cs[j].CPUPercent != nil {
			b = *cs[j].CPUPercent
		}
		return a > b
	})
	total := uint64(0)
	if sm.Host.MemoryTotal != nil {
		total = *sm.Host.MemoryTotal
	}
	rows := make([]table.Row, 0, len(cs))
	for n, c := range cs {
		marker := " "
		if n == s.table.Cursor() {
			marker = st.accent.Render(glyphCursor)
		}
		name, known := s.label(c.ID)
		nameStyle := st.text
		if !known {
			nameStyle = st.faint
		}
		cpu, cpuStyle := st.faint.Render("-"), st.faint
		if c.CPUPercent != nil {
			cpuStyle = st.text
			switch {
			case *c.CPUPercent >= 90:
				cpuStyle = st.err
			case *c.CPUPercent >= 60:
				cpuStyle = st.warn
			}
			cpu = cpuStyle.Render(fmt.Sprintf("%.1f%%", *c.CPUPercent))
		}
		limit := "∞"
		if c.MemoryLimit > 0 && (total == 0 || c.MemoryLimit < total*2) {
			limit = compactBytes(c.MemoryLimit)
		}
		mem := st.text.Render(compactBytes(c.MemoryUsage)) + st.faint.Render("/") + st.muted.Render(limit)
		if c.Error != "" {
			mem = st.err.Render("error")
		}
		rows = append(rows, table.Row{
			marker, nameStyle.Render(sanitize(name)), cpu, mem,
			st.muted.Render("↓" + compactBytes(c.NetworkRXBytes) + " ↑" + compactBytes(c.NetworkTXBytes)),
			st.muted.Render("r" + compactBytes(c.BlockReadBytes) + " w" + compactBytes(c.BlockWriteBytes)),
		})
	}
	return rows
}

// barLine is a label, a bar and its numbers on one line w cells wide.
func (s *serverModel) barLine(st styles, name, label, numbers string, w int, animate bool) string {
	const labelW = 8
	nw := lipgloss.Width(numbers)
	// The line is the label, the bar, a space, the percentage (4 cells), a space and the numbers.
	bw := max(w-labelW-nw-6, 6)
	m := s.meters[name]
	bar := st.faint.Render(strings.Repeat("░", bw))
	pct := st.faint.Render("  n/a")
	if m != nil {
		bar = m.view(bw, animate)
		pct = fmt.Sprintf("%3.0f%%", m.target*100)
		switch {
		case m.target >= 0.9:
			pct = st.err.Render(pct)
		case m.target >= 0.7:
			pct = st.warn.Render(pct)
		default:
			pct = st.text.Render(pct)
		}
	}
	return st.muted.Render(fit(label, labelW)) + bar + " " + pct + " " + st.muted.Render(numbers)
}

// hostLines are the meters of the host: CPU with its history, memory, swap, load and traffic.
func (s *serverModel) hostLines(st styles, w int, animate bool) []string {
	sm, ok := s.latest()
	if !ok {
		return nil
	}
	h := sm.Host
	var lines []string
	lines = append(lines, s.barLine(st, "cpu", "CPU", "", w, animate))
	// The history of the CPU: the newest ten minutes or so.
	values := make([]float64, 0, len(s.samples))
	for _, x := range s.samples {
		v := 0.0
		if x.Host.CPUPercent != nil {
			v = *x.Host.CPUPercent
		}
		values = append(values, v)
	}
	span := fmt.Sprintf("%dm", max(len(values)*10/60, 0))
	spark := sparkline(values, max(w-8-lipgloss.Width(span)-2, 4), 100)
	lines = append(lines, st.muted.Render(fit("history", 8))+st.info.Render(spark)+" "+st.faint.Render(span))
	if used, tot, ok := memoryUsed(h.MemoryTotal, h.MemoryAvailable); ok {
		lines = append(lines, s.barLine(st, "mem", "Memory", compactBytes(used)+"/"+compactBytes(tot), w, animate))
	}
	if used, tot, ok := memoryUsed(h.SwapTotal, h.SwapFree); ok {
		lines = append(lines, s.barLine(st, "swap", "Swap", compactBytes(used)+"/"+compactBytes(tot), w, animate))
	} else if h.SwapTotal != nil {
		lines = append(lines, st.muted.Render(fit("Swap", 8))+st.faint.Render("none"))
	}
	load := st.faint.Render("n/a")
	if len(h.Load) >= 3 {
		load = st.text.Render(fmt.Sprintf("%.2f  %.2f  %.2f", h.Load[0], h.Load[1], h.Load[2])) + st.faint.Render("  (1m 5m 15m)")
	}
	lines = append(lines, st.muted.Render(fit("Load", 8))+load)
	if rx, tx, rd, wr, ok := s.rates(); ok {
		lines = append(lines, st.muted.Render(fit("Network", 8))+st.text.Render("↓ "+rateText(rx)+"  ↑ "+rateText(tx))+
			st.faint.Render("   disk ")+st.text.Render("r "+rateText(rd)+"  w "+rateText(wr)))
	}
	return lines
}

// mountLabel is a mount point cut to the label column, keeping its end, which is the part that tells mounts apart.
func mountLabel(path string) string {
	r := []rune(sanitize(path))
	if len(r) <= 7 {
		return string(r)
	}
	return "…" + string(r[len(r)-6:])
}

// diskLines are a bar for each mount and a line for what Docker uses.
func (s *serverModel) diskLines(st styles, w int, animate bool) []string {
	var lines []string
	for _, mt := range s.mounts() {
		if mt.Total == 0 {
			continue
		}
		used := mt.Total - min(mt.Available, mt.Total)
		lines = append(lines, s.barLine(st, "mount:"+mt.Path, mountLabel(mt.Path), compactBytes(used)+"/"+compactBytes(mt.Total), w, animate))
	}
	if len(lines) == 0 {
		lines = append(lines, st.faint.Render("no disk data"))
	}
	if s.df != nil && s.df.DockerStatus == "available" {
		d := s.df.Docker
		var volumes, cache, writable int64
		for _, v := range d.Volumes {
			volumes += v.UsageData.Size
		}
		for _, b := range d.BuildCache {
			cache += b.Size
		}
		for _, c := range d.Containers {
			writable += c.SizeRw
		}
		sep := st.faint.Render(" " + glyphDot + " ")
		first := st.text.Render("images "+api.HumanSize(d.LayersSize)) + sep + st.text.Render("volumes "+api.HumanSize(volumes))
		second := st.text.Render("cache "+api.HumanSize(cache)) + sep + st.text.Render("containers "+api.HumanSize(writable))
		if lipgloss.Width(first)+lipgloss.Width(second)+lipgloss.Width(sep)+8 <= w {
			lines = append(lines, st.muted.Render(fit("Docker", 8))+first+sep+second)
		} else {
			lines = append(lines, st.muted.Render(fit("Docker", 8))+first, strings.Repeat(" ", 8)+second)
		}
	}
	return lines
}

// status is the right-hand part of the pane title.
func (s *serverModel) status(st styles, spin string) string {
	switch {
	case s.state == streamRetrying:
		why := s.text
		if why == "" {
			why = "stream ended"
		}
		return st.err.Render(fmt.Sprintf("%s %s %s retrying in %s", glyphFail, clip(sanitize(why), 40), glyphDot, api.HumanAge(s.retry)))
	case !s.live:
		return spin + st.muted.Render(" connecting")
	}
	return st.ok.Render(glyphHealthy+" live") + st.faint.Render(" "+glyphDot+" ") + st.muted.Render("every 10s")
}
