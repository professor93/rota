package client

import (
	"os"
	"testing"

	"github.com/professor93/rota/internal/fakecli"
)

// TestMain plays a fake CLI when this binary was started as one: the
// terminals these tests open run the tty fake as claude.
func TestMain(m *testing.M) {
	fakecli.Maybe()
	os.Exit(m.Run())
}
