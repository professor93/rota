package api

import "time"

// attendAll looks at every live held session once, at now, and says which
// of them want their person. It is keepAttending's tick, and says nothing
// until the attention events are written.
func (s *Server) attendAll(now time.Time) {}
