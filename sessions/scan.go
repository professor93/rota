package sessions

import (
	"path/filepath"
	"slices"
	"strconv"

	"github.com/professor93/rota/store"
)

// Report is everything rota can say about what the vendor CLIs are doing.
type Report struct {
	Instances []Instance `json:"instances,omitempty"`
	Sessions  []Session  `json:"sessions,omitempty"`
	Shared    *Shared    `json:"shared,omitzero"`

	// Notes say what could not be looked at and why. A gap with a reason is
	// worth more than a shorter list that quietly leaves things out.
	Notes []string `json:"notes,omitempty"`
}

// Shared is what sits in the person's own Claude Code home, which every
// account without a project of its own reads, and which therefore belongs to
// none of them.
type Shared struct {
	Dir      string `json:"dir"`
	Sessions int    `json:"sessions"`
	Projects int    `json:"projects"`
}

// RegistryFor is where rota writes down the runs it started: beside the
// accounts, not inside the per-account homes, because it is about rota.
func RegistryFor(st *store.Store) *Registry {
	return &Registry{Dir: filepath.Dir(st.Backend().HomeRoot())}
}

// Scan reports the CLIs running now and the conversations that could be
// resumed, keeping the newest few per account.
//
// A home shared by several accounts is read once. Two Claude Code accounts
// with no project of their own see the same thousands of transcripts, and
// listing those under each account would say the work happened twice.
func Scan(st *store.Store, recent int) Report {
	var rep Report

	// What rota started, which is the only source that knows the account.
	mine := RegistryFor(st).Running()
	rep.Instances = append(rep.Instances, mine...)
	known := make([]int, 0, len(mine))
	for _, in := range mine {
		known = append(known, in.PID)
	}

	// What is running that rota did not start.
	others, note := ProcessInstances(known)
	rep.Instances = append(rep.Instances, others...)
	if note != "" {
		rep.Notes = append(rep.Notes, note)
	}

	var unknownProviders []string
	seen := map[string]bool{}
	for _, a := range st.Accounts {
		if !readable(a.Provider) && !slices.Contains(unknownProviders, a.Provider) {
			unknownProviders = append(unknownProviders, a.Provider)
		}
		home, shared := ConfigHome(a, st.Home(a))
		if home == "" || seen[home] {
			continue
		}
		seen[home] = true

		for _, in := range IDEInstances(home) {
			if !shared {
				in.Account, in.Label, in.Provider = a.ID, a.Label(), a.Provider
			}
			rep.Instances = append(rep.Instances, in)
		}

		found, total, err := In(a, home, recent)
		if err != nil {
			rep.Notes = append(rep.Notes, "#"+strconv.Itoa(a.ID)+": "+err.Error())
			continue
		}
		if shared {
			// Nobody owns these, so they are summarised rather than filed
			// under whichever account happened to be read first.
			for i := range found {
				found[i].Account, found[i].Label, found[i].Shared = 0, "", true
			}
			rep.Shared = &Shared{Dir: home, Sessions: total, Projects: projectsIn(home)}
		}
		rep.Sessions = append(rep.Sessions, found...)
	}
	if len(unknownProviders) > 0 {
		rep.Notes = append(rep.Notes,
			"no session store rota can read for "+join(unknownProviders)+"; their conversations are not listed")
	}
	slices.SortFunc(rep.Sessions, func(x, y Session) int { return y.At.Compare(x.At) })
	return rep
}

// projectsIn counts the distinct directories a home has been used from. It
// counts the folders Claude Code files transcripts under rather than the
// sessions read, so a limit does not change the answer.
func projectsIn(home string) int {
	dirs, err := readDirNames(filepath.Join(home, "projects"))
	if err != nil {
		return 0
	}
	return len(dirs)
}

func join(names []string) string {
	switch len(names) {
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	}
	return names[0] + ", " + join(names[1:])
}
