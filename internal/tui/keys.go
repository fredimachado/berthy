package tui

import "charm.land/bubbles/v2/key"

// keyMap holds every binding of the interface. A binding that does something in the current view is shown in the help; the
// actions (restart, roll back, approve, reject) are shown only where they apply.
type keyMap struct {
	NextTab, PrevTab, GoTab  key.Binding
	Up, Down, PageUp, PageDn key.Binding
	Top, Bottom              key.Binding
	Open, Back               key.Binding
	Filter, Apply, Clear     key.Binding
	Follow, Replica, Refresh key.Binding
	Logs                     key.Binding
	Help, Quit               key.Binding
	// The actions: they are shown only in the views where they apply.
	Restart, Rollback, Approve, Reject key.Binding
}

func newKeyMap() keyMap {
	return keyMap{
		NextTab:  key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "next tab")),
		PrevTab:  key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("shift+tab", "previous tab")),
		GoTab:    key.NewBinding(key.WithKeys("1", "2", "3", "4"), key.WithHelp("1-4", "go to tab")),
		Up:       key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:     key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		PageUp:   key.NewBinding(key.WithKeys("pgup", "ctrl+u"), key.WithHelp("pgup", "page up")),
		PageDn:   key.NewBinding(key.WithKeys("pgdown", "ctrl+d"), key.WithHelp("pgdn", "page down")),
		Top:      key.NewBinding(key.WithKeys("home", "g"), key.WithHelp("g/home", "top")),
		Bottom:   key.NewBinding(key.WithKeys("end", "G"), key.WithHelp("G/end", "bottom")),
		Open:     key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open")),
		Back:     key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
		Filter:   key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
		Apply:    key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "apply")),
		Clear:    key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "clear")),
		Follow:   key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "follow")),
		Replica:  key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "replica")),
		Refresh:  key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		Logs:     key.NewBinding(key.WithKeys("l"), key.WithHelp("l", "logs")),
		Help:     key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:     key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Restart:  key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "restart")),
		Rollback: key.NewBinding(key.WithKeys("b"), key.WithHelp("b", "roll back")),
		Approve:  key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "approve")),
		Reject:   key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "reject")),
	}
}

// helpView says which help the footer shows: what the keys do depends on the view.
type helpView int

const (
	helpApps helpView = iota
	helpFilter
	helpDetail
	helpLogsPick
	helpLogs
	helpServer
	helpApprovals
	helpApprovalDetail
	helpListFilter
)

// contextKeys is the help.KeyMap of one view.
type contextKeys struct {
	k    keyMap
	view helpView
}

func (c contextKeys) ShortHelp() []key.Binding {
	k := c.k
	switch c.view {
	case helpFilter, helpListFilter:
		return []key.Binding{k.Apply, k.Clear}
	case helpDetail:
		return []key.Binding{k.Up, k.Restart, k.Rollback, k.Logs, k.Back, k.Help, k.Quit}
	case helpLogsPick:
		return []key.Binding{k.Up, k.Open, k.Filter, k.NextTab, k.Help, k.Quit}
	case helpLogs:
		return []key.Binding{k.Up, k.Follow, k.Replica, k.Filter, k.Back, k.Help, k.Quit}
	case helpServer:
		return []key.Binding{k.NextTab, k.Refresh, k.Help, k.Quit}
	case helpApprovals:
		return []key.Binding{k.Up, k.Open, k.Approve, k.Reject, k.Filter, k.Help, k.Quit}
	case helpApprovalDetail:
		return []key.Binding{k.Up, k.Approve, k.Reject, k.Back, k.Help, k.Quit}
	}
	return []key.Binding{k.Up, k.Open, k.Restart, k.Rollback, k.Filter, k.Help, k.Quit}
}

func (c contextKeys) FullHelp() [][]key.Binding {
	k := c.k
	// The full help lists the actions wherever you are; the short help only where they apply.
	for _, b := range []*key.Binding{&k.Restart, &k.Rollback, &k.Approve, &k.Reject} {
		b.SetEnabled(true)
	}
	return [][]key.Binding{
		{k.NextTab, k.PrevTab, k.GoTab, k.Help, k.Quit},
		{k.Up, k.Down, k.PageUp, k.PageDn, k.Top, k.Bottom},
		{k.Open, k.Back, k.Filter, k.Logs},
		{k.Follow, k.Replica, k.Refresh},
		{k.Restart, k.Rollback, k.Approve, k.Reject},
	}
}
