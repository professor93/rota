package notify

import (
	"errors"

	"github.com/professor93/rota/api"
)

// newWebhook makes a webhook sink. Its error may name the secret file but
// never quotes what is in it (newSender's contract).
func newWebhook(api.EventSink, env) (sender, error) {
	return nil, errors.New("webhook sinks are not built yet")
}
