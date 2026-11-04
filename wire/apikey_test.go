package wire

import (
	"encoding/json"
	"strings"
	"testing"

	rota "github.com/professor93/rota/lib"
)

// An account that runs on an API key says so, and says nothing else of the
// key: not the key, and no usage it does not have.
func TestDescribeSaysAnAccountRunsOnAKeyAndNeverShowsIt(t *testing.T) {
	const key = "sk-ant-api03-TESTONLY-not-a-real-key-0000000000-WXYZ"
	a := &rota.Account{ID: 7, Provider: "claude", UUID: "apikey-0011223344556677", Credential: "apikey",
		Token: rota.Token{Access: key}}
	raw, err := json.Marshal(Describe(a))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	if strings.Contains(doc, "TESTONLY") {
		t.Fatalf("the key itself must never be rendered: %s", doc)
	}
	if !strings.Contains(doc, `"credential":"apikey"`) || !strings.Contains(doc, `"metered":false`) {
		t.Fatalf("the kind of credential, and no usage to read: %s", doc)
	}
	a.Name = "work credit"
	if v := Describe(a); v.Name != "work credit" {
		t.Fatalf("a name it was given is carried: %+v", v)
	}
	o := &rota.Account{ID: 1, Provider: "claude", Email: "a@b.c"}
	raw, _ = json.Marshal(Describe(o))
	if strings.Contains(string(raw), `"credential"`) || strings.Contains(string(raw), `"name"`) || !Describe(o).Metered {
		t.Fatalf("an ordinary account is rendered as it always was: %s", raw)
	}
}
