package wire

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
)

// The caps on uploads are the staging's own, so every way in — multipart
// or JSON — meets the same limit before a byte is written.
func TestUploadsAreCappedWhereverTheyComeFrom(t *testing.T) {
	small := base64.StdEncoding.EncodeToString([]byte("x"))
	var many []Upload
	for i := range MaxUploads + 1 {
		many = append(many, Upload{Path: "f" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".txt", Content: small})
	}
	dir, err := StageUploads(many)
	if dir != "" {
		os.RemoveAll(dir)
	}
	if err == nil || !strings.Contains(err.Error(), "files") {
		t.Fatalf("%d files must be refused: %v", len(many), err)
	}
	// One byte over the per-file limit, declared by its encoded length,
	// is refused before it is decoded.
	big := Upload{Path: "big.bin", Content: base64.StdEncoding.EncodeToString(make([]byte, MaxUploadBytes+1))}
	dir, err = StageUploads([]Upload{big})
	if dir != "" {
		os.RemoveAll(dir)
	}
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("an oversize file must be refused: %v", err)
	}
}
