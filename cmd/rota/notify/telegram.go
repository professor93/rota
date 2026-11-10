package notify

import (
	"errors"

	"github.com/professor93/rota/api"
)

// telegramAPI is where Telegram's Bot API is, a variable so a test can
// stand in for it. It is not a key of server.toml.
var telegramAPI = "https://api.telegram.org"

// newTelegram makes a Telegram sink.
func newTelegram(api.EventSink, env) (sender, error) {
	return nil, errors.New("telegram sinks are not built yet")
}
