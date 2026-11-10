package notify

import (
	"errors"

	"github.com/professor93/rota/api"
)

// newWebhook makes a webhook sink.
func newWebhook(api.EventSink, env) (sender, error) {
	return nil, errors.New("webhook sinks are not built yet")
}
