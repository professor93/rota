package wire

import (
	"reflect"
	"testing"

	rota "github.com/professor93/rota/lib"
)

// A reply's Login is rota.Login less its state, field for field: a field added
// to rota.Login and not here would be missing from every reply, said nowhere.
func TestALoginReplyCarriesEveryFieldButTheState(t *testing.T) {
	tags := func(typ reflect.Type) map[string]string {
		m := map[string]string{}
		for i := range typ.NumField() {
			f := typ.Field(i)
			m[f.Name] = f.Tag.Get("json")
		}
		return m
	}
	want := tags(reflect.TypeFor[rota.Login]())
	if _, ok := want["State"]; !ok {
		t.Fatal("rota.Login has no State, which is what this reply leaves out")
	}
	delete(want, "State")
	if got := tags(reflect.TypeFor[Login]()); !reflect.DeepEqual(got, want) {
		t.Fatalf("wire.Login's fields and tags\n got %v\nwant %v", got, want)
	}

	full := &rota.Login{ID: "i", Provider: "p", URL: "u", Kind: "k", State: map[string]string{"verifier": "v"},
		Delegated: true, Long: true, Account: 3, CreatedAt: 9}
	if got := DescribeLogin(full); got != (Login{ID: "i", Provider: "p", URL: "u", Kind: "k", Delegated: true, Long: true, Account: 3, CreatedAt: 9}) {
		t.Fatalf("not copied whole: %+v", got)
	}
}
