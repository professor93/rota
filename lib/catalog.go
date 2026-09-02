package rota

import (
	"slices"
	"strings"
)

// Model is one model a provider accepts.
type Model struct {
	ID      string   `json:"id"`
	Aliases []string `json:"aliases,omitempty"`
	Label   string   `json:"label,omitempty"`
}

// Catalog is implemented by providers that know which models and reasoning
// efforts they accept. It is optional: a provider without one passes
// whatever it is given straight through, which is the right behaviour for a
// CLI whose model list rota cannot know.
//
// Keeping this on the provider rather than in one table is what lets a
// third-party provider ship its own models without touching rota.
type Catalog interface {
	// Models lists what this provider accepts, best first.
	Models() []Model
	// Efforts lists reasoning-effort levels, cheapest first. Nil means the
	// provider has no such setting, and asking for one is an error rather
	// than a silently dropped flag.
	Efforts() []string
	// Defaults are the model and effort used when a request names none. A
	// mid-range pair is the right choice: predictable cost, predictable
	// quality, and immune to the CLI changing its own default.
	Defaults() (model, effort string)
}

// AccountCatalog is implemented by providers whose model list belongs to
// the account rather than the provider. Codex is one: a ChatGPT login may
// use only the models its plan includes, and the CLI caches that list inside
// the account's own home. Refusing a model the account cannot use is worth
// doing here, because the provider only refuses it after the session has
// started.
type AccountCatalog interface {
	// ModelsFor lists what this account may use, or nil to fall back to the
	// provider's own list.
	ModelsFor(a *Account, home string) []Model
}

// ModelsFor lists the models one account may use. home is that account's
// private CLI directory, which is where a provider looks for a cached
// entitlement list; "" means "whatever the provider publishes".
func ModelsFor(a *Account, home string) []Model {
	if ac, ok := catalogOf(a.Provider).(AccountCatalog); ok && home != "" {
		if models := ac.ModelsFor(a, home); len(models) > 0 {
			return models
		}
	}
	return Models(a.Provider)
}

func catalogOf(provider string) Catalog {
	p, err := Lookup(provider)
	if err != nil {
		return nil
	}
	c, _ := p.(Catalog)
	return c
}

// copyModels hands a catalog out as the caller's own, alias slices included,
// so nothing written into it reaches the next caller.
func copyModels(src []Model) []Model {
	out := make([]Model, len(src))
	for i, m := range src {
		out[i] = m
		out[i].Aliases = append([]string(nil), m.Aliases...)
	}
	return out
}

// Models lists a provider's models, or nil when it publishes none.
func Models(provider string) []Model {
	if c := catalogOf(provider); c != nil {
		return c.Models()
	}
	return nil
}

// Efforts lists a provider's reasoning-effort levels, or nil when it has no
// such setting.
func Efforts(provider string) []string {
	if c := catalogOf(provider); c != nil {
		return c.Efforts()
	}
	return nil
}

// Defaults are the model and effort a provider uses when none is named.
func Defaults(provider string) (model, effort string) {
	if c := catalogOf(provider); c != nil {
		return c.Defaults()
	}
	return "", ""
}

// ResolveModel turns what a caller asked for into the id its provider
// expects: "" becomes the provider's default, an alias becomes its full id,
// and a model belonging to some other provider is an error rather than a
// request that fails halfway through a paid session.
func ResolveModel(provider, want string) (string, error) {
	return resolveModel(provider, want, Models(provider))
}

func resolveModel(provider, want string, models []Model) (string, error) {
	if len(models) == 0 {
		return want, nil // no catalog: the CLI is the judge
	}
	if want == "" {
		def, _ := Defaults(provider)
		return def, nil
	}
	for _, m := range models {
		if strings.EqualFold(m.ID, want) {
			return m.ID, nil
		}
		for _, alias := range m.Aliases {
			if strings.EqualFold(alias, want) {
				return m.ID, nil
			}
		}
	}
	if p, err := Lookup(provider); err == nil {
		if fc, ok := p.(FloorCatalog); ok && fc.CatalogIsFloor() {
			// The floor lets an id beyond the list through for the CLI to
			// judge — but an id that belongs to a different provider's
			// catalog is a mix-up, not a newer model, and refusing it here
			// is cheaper than a paid session failing halfway.
			for _, other := range Providers() {
				if other == provider {
					continue
				}
				for _, m := range Models(other) {
					if strings.EqualFold(m.ID, want) || slices.ContainsFunc(m.Aliases, func(a string) bool { return strings.EqualFold(a, want) }) {
						return "", failf(ErrInvalidRequest, "%q is %s's model, and this account is %s", want, other, provider)
					}
				}
			}
			return want, nil // the list is the floor, not the ceiling
		}
	}
	known := make([]string, 0, len(models))
	for _, m := range models {
		known = append(known, m.ID)
	}
	return "", failf(ErrInvalidRequest, "%s has no model %q; it accepts: %s", provider, want, strings.Join(known, ", "))
}

// ResolveEffort validates a reasoning effort against its provider: "" becomes
// the default, an unknown level is an error, and any level at all is an
// error for a provider that has no such setting.
func ResolveEffort(provider, want string) (string, error) {
	levels := Efforts(provider)
	if len(levels) == 0 {
		if want != "" {
			return "", failf(ErrInvalidRequest, "%s has no reasoning-effort setting; drop the effort field", provider)
		}
		return "", nil
	}
	if want == "" {
		_, def := Defaults(provider)
		return def, nil
	}
	for _, l := range levels {
		if strings.EqualFold(l, want) {
			return l, nil
		}
	}
	return "", failf(ErrInvalidRequest, "%s has no effort %q; it accepts: %s", provider, want, strings.Join(levels, ", "))
}

// FloorCatalog marks a provider whose model list is known to be incomplete —
// the floor rather than the whole truth — so an id beyond it passes through
// for the CLI to judge instead of being refused. A provider with a complete
// catalog leaves this unimplemented and keeps typo-before-spend protection.
type FloorCatalog interface {
	CatalogIsFloor() bool
}

// LoginPlanFor is how to sign a delegated account in, and whether there is
// anything to run at all.
func LoginPlanFor(a *Account, home string) (LoginPlan, bool) {
	if !a.Delegated {
		return LoginPlan{}, false
	}
	p, err := Lookup(a.Provider)
	if err != nil {
		return LoginPlan{}, false
	}
	d, ok := p.(Delegator)
	if !ok {
		return LoginPlan{}, false
	}
	return d.LoginPlan(a, home), true
}

// PermissionModes are the values a provider's CLI accepts for
// permission_mode. Empty means it has no such setting — codex governs the
// same ground with a sandbox and an approval flag instead.
//
// This lives here, beside Models and Efforts, for the same reason: what a
// CLI accepts is the library's to know, and a form that guessed would offer
// a value the run then refuses.
func PermissionModes(provider string) []string {
	switch Flavor(provider) {
	case "claude":
		return append([]string(nil), permModes...)
	case "grok":
		return append([]string(nil), grokPermModes...)
	case "kimi":
		return append([]string(nil), kimiPermModes...)
	}
	return nil
}

// Sandboxes are the sandbox profiles a provider's CLI accepts. Empty does
// not mean "no sandbox": grok takes a profile name its own configuration
// may define, which rota cannot enumerate. Ask TakesSandbox for that.
func Sandboxes(provider string) []string {
	if Flavor(provider) == "codex" {
		return append([]string(nil), sandboxes...)
	}
	return nil
}

// TakesSandbox reports whether a provider has a sandbox setting at all.
func TakesSandbox(provider string) bool {
	switch Flavor(provider) {
	case "codex", "grok":
		return true
	}
	return false
}
