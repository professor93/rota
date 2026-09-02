package store

import (
	"os"
	"testing"

	"github.com/professor93/rota/internal/fakecli"
)

// The test binary doubles as every fake vendor CLI the tests install.
func TestMain(m *testing.M) {
	fakecli.Maybe()
	os.Exit(m.Run())
}
