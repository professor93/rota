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
// Many files share this one package. A file declares no top-level name
// beyond what its task's interface names, except names that begin with the
// file's own stem (harnessToy in harness_test.go, themeColor in theme.go),
// so files written side by side never collide.
package tui
