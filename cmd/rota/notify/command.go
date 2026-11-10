package notify

import (
	"errors"

	"github.com/professor93/rota/api"
)

// newCommand makes a command sink.
func newCommand(api.EventSink, env) (sender, error) {
	return nil, errors.New("command sinks are not built yet")
}
