package tui

import tea "charm.land/bubbletea/v2"

// How the shell opens an overlay over the tabs and keeps its stack: what it
// opens, in what order, what it starts as it opens, and what it drops.

// modelAbout is an overlay that says what it is about: its kind and the
// account it acts on. One that does not say is never taken for another.
type modelAbout interface {
	About() (kind string, account int)
}

// modelStarter is an overlay that starts something as it opens: the login
// flow on l begins a login that needs no choice. Its Init runs once, when
// it is put on the stack, and never for one that is not opened.
type modelStarter interface {
	Init() tea.Cmd
}

// open puts o on top of the stack and starts it, unless the overlay on top
// is about the same: a key pressed twice before its overlay arrived (a
// double d) opens one. A login flow opened while one is put aside brings
// that one back and is dropped: one flow at a time (R17).
func (m *Model) open(o overlay) tea.Cmd {
	if o == nil || o.Closed() {
		return nil
	}
	if n := len(m.stack); n > 0 {
		t, ok1 := m.stack[n-1].(modelAbout)
		u, ok2 := o.(modelAbout)
		if ok1 && ok2 {
			tk, ta := t.About()
			if uk, ua := u.About(); tk == uk && ta == ua {
				return nil
			}
		}
	}
	if _, ok := o.(asider); ok {
		if m.aside != nil {
			m.aside.Return()
			m.stack = append(m.stack, m.aside)
			m.aside = nil
			m.settle()
			return nil
		}
		for _, s := range m.stack {
			if _, open := s.(asider); open {
				return nil
			}
		}
	}
	m.stack = append(m.stack, o)
	var start tea.Cmd
	if s, ok := o.(modelStarter); ok {
		start = s.Init()
	}
	m.settle()
	return start
}

// opened opens, in order and as openMsg would, what views asked to open,
// and gives back what those it opened start.
func (m *Model) opened() tea.Cmd {
	var cmds []tea.Cmd
	for ; len(m.e.opening) > 0; m.e.opening = m.e.opening[1:] {
		cmds = append(cmds, m.open(m.e.opening[0]))
	}
	return tea.Batch(cmds...)
}

// settle drops every closed overlay, takes a flow that says it is aside
// off the stack, and puts one that came back on top.
func (m *Model) settle() {
	var kept []overlay
	for _, o := range m.stack {
		if o.Closed() {
			continue
		}
		if a, ok := o.(asider); ok && a.Aside() {
			m.aside = a
			continue
		}
		kept = append(kept, o)
	}
	m.stack = kept
	if m.aside != nil && m.aside.Closed() {
		m.aside = nil
	}
	if m.aside != nil && !m.aside.Aside() {
		m.stack = append(m.stack, m.aside)
		m.aside = nil
	}
}
