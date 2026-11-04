package machine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadSerial(t *testing.T) {
	dir := t.TempDir()
	for name, c := range map[string]struct{ file, want string }{
		"a Raspberry Pi": {"10000000a1b2c3d4\x00", "10000000a1b2c3d4\x00"},
		"far too long":   {strings.Repeat("a", 10*maxRead), strings.Repeat("a", maxRead)},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, "serial-number")
			if err := os.WriteFile(path, []byte(c.file), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := readSerial(path); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
	if got := readSerial(filepath.Join(dir, "none")); got != "" {
		t.Fatalf("no file gave %q", got)
	}
}
