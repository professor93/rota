// Package tui is the terminal interface bare `rota` opens: the Accounts and
// Ask tabs, in the Elm architecture of Bubble Tea v2.
//
// Update does no I/O; every effect is a tea.Cmd. key.Binding is the only
// source of keys, so the help cannot drift from what the keys do. One theme
// (theme.go) holds every colour, for light and dark terminals at truecolor,
// 256 and 16 colours, and none at all under NO_COLOR.
//
// Text inputs come only from newInput and newArea, never from textinput.New
// or textarea.New: theirs have a cursor that does not blink, so the tests'
// driver goes quiet, and no ctrl+v or ctrl+shift+c that would read or write
// the clipboard of the machine the TUI runs on, which over SSH is the
// daemon's. A paste arrives only as tea.PasteMsg.
//
// # Names
//
// Many files share this one package. A file declares no top-level name
// beyond what its task's interface names, except names that begin with the
// file's own stem (harnessToy in harness_test.go, themeColor in theme.go),
// so files written side by side never collide. A test function the plan
// names keeps that name; any other begins with Test and the stem
// (TestHarness..., TestTheme...). TestMain is harness_test.go's.
//
// # Tests
//
// Tests run headless, through the driver in harness_test.go:
//
//	m, seen := run(t, m, msgs...)   // deliver msgs and run what comes back
//	m, seen = await(t, m, msgs...)  // first wait for a long-running command
//
// run takes the test: each test has its own commands, a command one test
// leaves running never reaches another, and a test that ends with one still
// running fails, naming it. A round waits at most 200 ms, and only for the
// commands started since the last delivery. A command still running after
// its round (a stream, a page's end) is long-running: no round waits for it
// again, so the hops beside it stay quick, and the first round that finds it
// returned delivers its message. A test that needs that message now calls
// await. A quit forgets whatever is still running, as the program's end
// does. Start a model with run(t, m, tea.BatchMsg{m.Init()}). Use tea.Batch,
// never tea.Sequence, whose message the driver cannot take apart.
//
// Timers come from fakeTicks (tick, advance, now) and fire only when a test
// advances its clock, one period at a time for a tick that is set again.
package tui
