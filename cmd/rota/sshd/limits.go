package sshd

import (
	"fmt"
	"sync"
	"time"

	"github.com/professor93/rota/api"
)

// One person's share, whatever their role: a quarter of the connections and
// half the attaches, so one key, even a watcher's, cannot hold every place.
const (
	maxConnsPerUser    = 16
	maxAttachesPerUser = 8
)

// The places kept for control: the last connections and the last attaches
// go to people whose role is control alone, so the others together, however
// many there are, cannot shut the owner out.
const (
	limitsControlConns    = 16
	limitsControlAttaches = 4
)

// limitsRefusedEvery is how often a refusal from one address is logged; the
// ones between are counted on the next line. A peer can connect and fail as
// fast as it likes, and the log is not its to fill. A test knob.
var limitsRefusedEvery = time.Minute

// limitsRefusedKept is how many addresses' refusal counts are kept. Past it,
// with none old enough to forget, every other address shares one more,
// limitsOverflow, so the log is limited still.
const limitsRefusedKept = 1024

// limitsOverflow is the address the refusals past limitsRefusedKept share.
const limitsOverflow = ""

// limits is what the signed-in hold, under Server.mu.
type limits struct {
	people         map[string]int // connections by person
	others         int            // connections of people without control
	attached       int
	attachAs       map[string]int // attaches by person
	othersAttached int            // attaches of people without control
	refused        map[string]*limitsRefusal
}

// limitsRefusal is one address's refusals since its last log line.
type limitsRefusal struct {
	at         time.Time
	suppressed int
}

func limitsNew() limits {
	return limits{people: map[string]int{}, attachAs: map[string]int{}, refused: map[string]*limitsRefusal{}}
}

// limitsAdmit counts a signed-in connection for its person, or says why it
// is refused; leave gives the place back.
func (s *Server) limitsAdmit(u User) (leave func(), why string) {
	other := u.Role != api.RoleControl
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.people[u.Name] >= maxConnsPerUser:
		return nil, fmt.Sprintf("too many connections (%d)", maxConnsPerUser)
	case other && s.others >= maxConns-limitsControlConns:
		return nil, fmt.Sprintf("the last %d connections are kept for control", limitsControlConns)
	}
	s.people[u.Name]++
	if other {
		s.others++
	}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.people[u.Name]--; s.people[u.Name] <= 0 {
			delete(s.people, u.Name)
		}
		if other {
			s.others--
		}
	}, ""
}

// limitsSlot takes one attach's place, on the connection, for the person and
// on the server, and gives back the func that returns it, once however
// often it is called.
func (c *sshdConn) limitsSlot() (func(), error) {
	s, name, other := c.s, c.user.Name, c.user.Role != api.RoleControl
	s.mu.Lock()
	defer s.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.attached >= maxAttachesPerConn:
		return nil, fmt.Errorf("rota over ssh has %d sessions attached on this connection; leave one first", maxAttachesPerConn)
	case s.attachAs[name] >= maxAttachesPerUser:
		return nil, fmt.Errorf("rota over ssh has %d sessions attached as %s; leave one first", maxAttachesPerUser, name)
	case s.attached >= maxAttaches:
		return nil, fmt.Errorf("rota over ssh has %d sessions attached; leave one first", maxAttaches)
	case other && s.othersAttached >= maxAttaches-limitsControlAttaches:
		return nil, fmt.Errorf("rota over ssh keeps the last %d attached sessions for control; leave one first", limitsControlAttaches)
	}
	s.attached++
	s.attachAs[name]++
	if other {
		s.othersAttached++
	}
	c.attached++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			c.mu.Lock()
			defer c.mu.Unlock()
			s.attached--
			if s.attachAs[name]--; s.attachAs[name] <= 0 {
				delete(s.attachAs, name)
			}
			if other {
				s.othersAttached--
			}
			c.attached--
		})
	}, nil
}

// limitsRefused logs a refusal from peer, at most once every s.refusing for
// each peer; the line says how many were not logged since the last one. A
// new peer with the table full is first given room by forgetting those whose
// window has passed, and the refusals they were holding back get a line of
// their own; with no room still, it shares limitsOverflow.
func (s *Server) limitsRefused(peer string, args ...any) {
	now := time.Now()
	forgotten := 0
	s.mu.Lock()
	r := s.refused[peer]
	if r == nil && len(s.refused) >= limitsRefusedKept {
		for p, old := range s.refused {
			if now.Sub(old.at) >= s.refusing {
				forgotten += old.suppressed
				delete(s.refused, p)
			}
		}
		if len(s.refused) >= limitsRefusedKept {
			peer = limitsOverflow
			r = s.refused[peer]
		}
	}
	logged, suppressed := true, 0
	switch {
	case r != nil && now.Sub(r.at) < s.refusing:
		r.suppressed++
		logged = false
	case r != nil:
		suppressed = r.suppressed
		fallthrough
	default:
		s.refused[peer] = &limitsRefusal{at: now}
	}
	s.mu.Unlock()
	if forgotten > 0 {
		s.log.Info("ssh refused", "addr", "others", "suppressed", forgotten)
	}
	if !logged {
		return
	}
	if suppressed > 0 {
		args = append(args, "suppressed", suppressed)
	}
	s.log.Info("ssh refused", args...)
}
