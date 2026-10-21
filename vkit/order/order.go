// Package order gives the fake devices of the virtual kit (docs/kit-virtuel.md) their
// remote control: a named pipe that takes one order per line, written by the
// scenario scripts (echo 'sms +22890000001 STATUT' > /run/vkit/modem).
package order

import (
	"bufio"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Listen creates the named pipe at path and hands each line to apply, split into
// words, in the background. A failed order is logged. The pipe is opened for
// reading and writing, so it never reaches its end when a writer leaves.
func Listen(path string, apply func(words []string) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	os.Remove(path)
	if err := syscall.Mkfifo(path, 0o620); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	go func() {
		s := bufio.NewScanner(f)
		for s.Scan() {
			words := strings.Fields(s.Text())
			if len(words) == 0 {
				continue
			}
			if err := apply(words); err != nil {
				log.Printf("order %q: %v", s.Text(), err)
			}
		}
	}()
	return nil
}
