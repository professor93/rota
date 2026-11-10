package notify

import (
	"errors"

	"github.com/professor93/rota/api"
)

// newCommand makes a command sink. Its error may name the program but never
// quotes a file it read (newSender's contract).
func newCommand(api.EventSink, env) (sender, error) {
	return nil, errors.New("command sinks are not built yet")
}
