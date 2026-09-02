package rota

import "testing"

// A catalog comes back as a copy all the way down: the alias slices inside
// each model are the caller's too, so writing into one cannot rename an
// alias for every later caller.
func TestModelCatalogsCopyAliasesToo(t *testing.T) {
	for _, name := range Providers() {
		p, _ := Lookup(name)
		c, ok := p.(Catalog)
		if !ok {
			continue
		}
		first := c.Models()
		for i := range first {
			if len(first[i].Aliases) > 0 {
				first[i].Aliases[0] = "tampered"
			}
		}
		for _, m := range c.Models() {
			for _, alias := range m.Aliases {
				if alias == "tampered" {
					t.Fatalf("%s: a caller's alias mutation reached the next caller", name)
				}
			}
		}
	}
}
