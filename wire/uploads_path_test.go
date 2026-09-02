package wire

import (
	"encoding/base64"
	"os"
	"runtime"
	"testing"
)

// An upload path is written with forward slashes wherever the caller runs,
// and judged in this platform's own form: "notes/a.txt" is plain on
// Windows too, while anything that climbs out or names a drive is not.
func TestUploadPathsAreJudgedInThePlatformsForm(t *testing.T) {
	content := base64.StdEncoding.EncodeToString([]byte("x"))
	for _, ok := range []string{"a.txt", "notes/a.txt", "deep/er/file.bin"} {
		dir, err := StageUploads([]Upload{{Path: ok, Content: content}})
		if dir != "" {
			os.RemoveAll(dir)
		}
		if err != nil {
			t.Fatalf("%q must be accepted: %v", ok, err)
		}
	}
	bad := []string{"", "../a", "a/../b", "./a", "a//b", "~/a", "/abs"}
	if runtime.GOOS == "windows" {
		bad = append(bad, `C:\a`, `..\a`, `\abs`)
	}
	for _, p := range bad {
		dir, err := StageUploads([]Upload{{Path: p, Content: content}})
		if dir != "" {
			os.RemoveAll(dir)
		}
		if err == nil {
			t.Fatalf("%q must be refused", p)
		}
	}
}
