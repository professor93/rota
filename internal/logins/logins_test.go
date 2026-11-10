package logins

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	rota "github.com/professor93/rota/lib"
)

// fields is a struct's field names and JSON tags, in order.
func fields(typ reflect.Type) []string {
	out := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		f := typ.Field(i)
		out = append(out, f.Name+" "+f.Tag.Get("json"))
	}
	return out
}

// A reply is rota.Login less its state, field for field, and the account it
// was begun for before CreatedAt: a field added to rota.Login and not here
// would be missing from every reply, said nowhere.
func TestAReplyCarriesEveryFieldButTheState(t *testing.T) {
	var want []string
	for _, f := range fields(reflect.TypeFor[rota.Login]()) {
		switch f {
		case `State state`:
			continue
		case `CreatedAt createdAt`:
			want = append(want, `Account account,omitzero`)
		}
		want = append(want, f)
	}
	if got := fields(reflect.TypeFor[Reply]()); !reflect.DeepEqual(got, want) {
		t.Fatalf("Reply's fields and tags\n got %v\nwant %v", got, want)
	}

	// Every field of a login set, by reflection, so that one added later is set
	// too: ReplyOf must carry each of them, by name.
	var full rota.Login
	v := reflect.ValueOf(&full).Elem()
	for i := range v.NumField() {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString(v.Type().Field(i).Name)
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Int, reflect.Int64:
			f.SetInt(int64(100 + i))
		case reflect.Map:
			f.Set(reflect.ValueOf(map[string]string{"verifier": "v"}))
		default:
			t.Fatalf("rota.Login.%s is a %s, which this test cannot fill", v.Type().Field(i).Name, f.Kind())
		}
	}
	got := reflect.ValueOf(ReplyOf(&full, 3))
	for i := range got.NumField() {
		name := got.Type().Field(i).Name
		if name == "Account" {
			if got.Field(i).Int() != 3 {
				t.Fatalf("the account: %v", got.Field(i))
			}
			continue
		}
		if want := v.FieldByName(name); !reflect.DeepEqual(got.Field(i).Interface(), want.Interface()) {
			t.Fatalf("%s: got %v, want %v", name, got.Field(i), want)
		}
	}

	fixed := &rota.Login{ID: "i", Provider: "p", URL: "u", Kind: "k", State: map[string]string{"verifier": "v"},
		Delegated: true, Long: true, CreatedAt: 9}
	raw, err := json.Marshal(ReplyOf(fixed, 3))
	if want := `{"id":"i","provider":"p","url":"u","kind":"k","delegated":true,"long":true,"account":3,"createdAt":9}`; err != nil || string(raw) != want {
		t.Fatalf("the bytes a reply is\n got %s %v\nwant %s", raw, err, want)
	}
	if raw, _ := json.Marshal(ReplyOf(&rota.Login{ID: "i"}, 0)); string(raw) != `{"id":"i","provider":"","url":"","kind":"","createdAt":0}` {
		t.Fatalf("what is not so is not said: %s", raw)
	}
}

func TestUnfinishableIsOnlyWhatIsMarked(t *testing.T) {
	refused := fmt.Errorf("%w: the account is gone", rota.ErrInvalidRequest)
	marked := MarkUnfinishable(refused)
	if !Unfinishable(marked) || !errors.Is(marked, rota.ErrInvalidRequest) || marked.Error() != refused.Error() {
		t.Fatalf("marked: %v", marked)
	}
	if Unfinishable(refused) || Unfinishable(nil) || Unfinishable(rota.ErrNoLogin) {
		t.Fatal("only a marked refusal is unfinishable")
	}
	if !Unfinishable(fmt.Errorf("finishing: %w", marked)) {
		t.Fatal("a wrapped mark is still the mark")
	}
}
