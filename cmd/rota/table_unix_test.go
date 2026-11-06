//go:build linux || darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/professor93/rota/internal/pty"
)

// A real rota listing at a real terminal of 80 columns asks the terminal
// how wide it is, and fits: no line it prints is wider, and the reason a
// dead account gives is all there, read down its column. A terminal that
// will not say, one that answers 0 columns, is fitted to COLUMNS instead.
func TestListAtATerminalFitsItsWidth(t *testing.T) {
	for _, c := range []struct {
		name    string
		cols    uint16
		columns string
	}{
		// COLUMNS says something else, so a listing that fits 80 is one
		// that asked the terminal.
		{"the terminal says", 80, "200"},
		// Without COLUMNS a terminal of 0 columns would get the table at its
		// natural width, far wider than 80.
		{"COLUMNS says", 0, "80"},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			now := time.Now()
			writeStore(t, home, fmt.Sprintf(`{"ordered":true,"nextId":3,"accounts":[
				{"id":1,"provider":"claude","email":"manager.ican@gmail.com","order":1,"token":{"accessToken":"t"},"quotaAt":%[1]d,
				 "quota":{"windows":[{"name":"5h","percent":3,"primary":true},{"name":"7d","percent":29},{"name":"Fable","percent":48,"scoped":true}]},
				 "extra":{"refresh_token_expires_at":"%[2]d"}},
				{"id":2,"provider":"claude","email":"b@x","order":2,"token":{"accessToken":"t"},"quotaAt":%[1]d,
				 "dead":true,"deadReason":%[3]q}]}`,
				now.UnixMilli(), now.Add(3*24*time.Hour+time.Hour).UnixMilli(), clearedReason))

			cmd := exec.Command(os.Args[0], "list")
			cmd.Env = append(os.Environ(), "ROTA_AS_RUN=1", "ROTA_HOME="+home, "COLUMNS="+c.columns)
			master, err := pty.Start(cmd, c.cols, 25)
			if err != nil {
				t.Fatalf("no pseudo-terminal: %v", err)
			}
			screen := watchScreen(master)
			t.Cleanup(func() {
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				_ = cmd.Wait()
				master.Close()
			})
			// The last thing it prints is the end of the login note.
			screen.wait(t, "then\r\n")

			out := strings.ReplaceAll(screen.text(), "\r\n", "\n")
			table, _, ok := strings.Cut(out, "┘\n")
			if !ok {
				t.Fatalf("a table:\n%s", out)
			}
			for _, line := range strings.Split(out, "\n") {
				if n := utf8.RuneCountInString(line); n > 80 {
					t.Fatalf("a line %d wide at an 80-column terminal: %q\n%s", n, line, out)
				}
			}
			framed(t, table[strings.Index(table, "┌"):]+"┘\n", 80)
			if got := column(table, "STATUS", "2"); squeeze(got) != squeeze("re-auth needed ("+clearedReason+")") {
				t.Fatalf("the reason, read down its column: %q\n%s", got, out)
			}
		})
	}
}
