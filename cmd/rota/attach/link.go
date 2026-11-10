package attach

import "github.com/professor93/rota/cmd/rota/client"

// source is where an attach finds sessions and opens them: the rota daemon,
// through a *client.Client, or a test's script.
type source interface {
	// open attaches to the session id, watching only when watch is set, and
	// has read its hello. A daemon's no is a *client.Refusal.
	open(id string, watch bool) (link, error)
	// list is every session the daemon holds or lists, running and stopped.
	list() ([]client.Session, error)
}

// link is one attach to one session: a *client.Term, or a test's script.
// Next is one goroutine's; the rest may be called from any.
type link interface {
	Hello() client.Hello
	Next() (client.Frame, error)
	Type([]byte) error
	Fit(cols, rows uint16) error
	Claim(force bool) error
	Close() error
}

// linkClient is a source over the daemon's own client.
type linkClient struct{ c *client.Client }

func (s linkClient) open(id string, watch bool) (link, error) {
	t, err := s.c.Attach(id, watch)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (s linkClient) list() ([]client.Session, error) { return s.c.List() }

// linkRedialer is a source that a redial replaces: the client Options.Redial
// gave is the one the attach goes on with. A source that is not one, a
// test's, goes on as it was.
type linkRedialer interface {
	redial(c *client.Client) source
}

func (s linkClient) redial(c *client.Client) source {
	if c == nil {
		return s
	}
	return linkClient{c}
}
