package store

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	rota "github.com/professor93/rota/lib"
)

// A config directory is where a run stages this account's credential, and
// what Remove deletes when it is rota's own — so one inside rota's own
// directories is refused: a sibling's home, the root the homes live in, the
// store itself, or anything that contains the store.
func TestAConfigDirInsideRotasOwnDirectoriesIsRefused(t *testing.T) {
	s := openTemp(t)
	a := s.add("codex")
	b := s.add("codex")
	for _, dir := range []string{
		s.Home(b),
		filepath.Join(s.Home(b), "deeper"),
		s.Backend().HomeRoot(),
		storeDir(s),
		filepath.Dir(storeDir(s)),
	} {
		a.ConfigDir = dir
		if err := s.CheckHome(a); !errors.Is(err, rota.ErrInvalidRequest) {
			t.Errorf("%s must be refused as rota's own, got %v", dir, err)
		}
	}
	own := filepath.Join(s.Backend().HomeRoot(), "codex-1")
	for _, dir := range []string{"", own, t.TempDir()} {
		a.ConfigDir = dir
		if err := s.CheckHome(a); err != nil {
			t.Errorf("%q must be allowed: %v", dir, err)
		}
	}
}

// A link is judged by where it points, even when the last part of the path
// does not exist yet: the sibling's home may not have been created.
func TestAConfigDirLinkedIntoRotasOwnDirectoriesIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege on windows")
	}
	s := openTemp(t)
	a := s.add("codex")
	link := filepath.Join(t.TempDir(), "homes")
	if err := os.Symlink(s.Backend().HomeRoot(), link); err != nil {
		t.Fatal(err)
	}
	a.ConfigDir = filepath.Join(link, "codex-2")
	if err := s.CheckHome(a); !errors.Is(err, rota.ErrInvalidRequest) {
		t.Fatalf("a link into the homes must be refused, got %v", err)
	}
}

// Given roots, a config directory must lie inside one of them; without any
// it may be anywhere that is not rota's own.
func TestAConfigDirIsConfinedToTheRootsWhenThereAreAny(t *testing.T) {
	s := openTemp(t)
	a := s.add("codex")
	root := t.TempDir()
	a.ConfigDir = filepath.Join(root, "cfg")
	if err := s.CheckHome(a, root); err != nil {
		t.Fatalf("inside a root: %v", err)
	}
	a.ConfigDir = t.TempDir()
	if err := s.CheckHome(a, root); !errors.Is(err, rota.ErrOutsideRoots) {
		t.Fatalf("outside every root must be refused, got %v", err)
	}
	if err := s.CheckHome(a); err != nil {
		t.Fatalf("no roots means unconfined: %v", err)
	}
}

// Removing an account deletes the home rota made for it and nothing else: a
// directory the person chose holds their memory and skills.
func TestRemoveDeletesOnlyAHomeThatIsRotasOwn(t *testing.T) {
	s := openTemp(t)
	a := s.add("codex")
	b := s.add("codex")
	c := s.add("codex")
	mine := t.TempDir()
	b.ConfigDir = mine
	c.ConfigDir = filepath.Join(s.Backend().HomeRoot(), "codex-3") // the default, spelled out
	for _, home := range []string{s.Home(a), mine, s.Home(c)} {
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []int{a.ID, b.ID, c.ID} {
		if err := s.Remove(id); err != nil {
			t.Fatal(err)
		}
		if s.Find(id) != nil {
			t.Fatalf("account %d must be gone", id)
		}
	}
	for _, home := range []string{s.Home(a), s.Home(c)} {
		if _, err := os.Stat(home); !os.IsNotExist(err) {
			t.Fatalf("rota's own home %s must go with its account: %v", home, err)
		}
	}
	if _, err := os.Stat(filepath.Join(mine, "auth.json")); err != nil {
		t.Fatalf("a directory the person chose must be left alone: %v", err)
	}
}
