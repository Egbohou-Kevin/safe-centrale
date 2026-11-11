package agentconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// A move to another cell of the cloud (agent.move) changes the cloud's addresses and
// the site's secret. The configuration file is read-only to the agent (/etc, see its
// systemd unit): the new cloud is written in the agent's own folder (KeyDir), where
// it overrides the file's. Three files, each 0600, each whole or absent:
//   - MovedName: the cloud the agent moved to, the current one;
//   - NextName: the cloud of a move under way, until cell B accepts the agent;
//   - PreviousName: the cloud it moved from, kept once.
//
// Each is written to a temporary file, flushed, renamed, and the folder flushed: a
// power cut between two writes leaves the old configuration as it was.
const (
	MovedName    = "cloud.json"
	NextName     = "cloud.next.json"
	PreviousName = "cloud.previous.json"
)

type movedFile struct {
	Ref    string   `json:"ref"` // the agent.move that wrote it
	URLs   []string `json:"urls"`
	Secret string   `json:"secret"`
}

// rename and syncFolder are os.Rename and syncDir, replaced in tests to stop between
// two writes.
var (
	rename     = os.Rename
	syncFolder = syncDir
)

// WriteNext writes the cloud of the move ref beside the current one, which it does
// not touch.
func WriteNext(dir, ref string, urls []string, secret string) error {
	for i, raw := range urls {
		if err := checkCloudURL(raw); err != nil {
			return fmt.Errorf("agentconfig: the new cloud's urls[%d]: %w", i, err)
		}
	}
	if ref == "" || len(urls) == 0 || secret == "" {
		return errors.New("agentconfig: the new cloud needs its move, its urls and its secret")
	}
	b, err := json.Marshal(movedFile{Ref: ref, URLs: urls, Secret: secret})
	if err != nil {
		return err
	}
	return writeAtomic(dir, NextName, b)
}

// Commit makes the cloud of the move the current one: the current one, if the agent
// had moved before, is kept as the previous one, then the new one takes its place.
// Stopped in between, the current one is untouched and the move is still pending.
func Commit(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, NextName)); err != nil {
		return fmt.Errorf("agentconfig: no move to commit: %w", err)
	}
	cur, err := os.ReadFile(filepath.Join(dir, MovedName))
	switch {
	case err == nil:
		if err := writeAtomic(dir, PreviousName, cur); err != nil {
			return err
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := rename(filepath.Join(dir, NextName), filepath.Join(dir, MovedName)); err != nil {
		return err
	}
	return syncFolder(dir)
}

// Discard forgets the cloud of a move that went back.
func Discard(dir string) error {
	err := os.Remove(filepath.Join(dir, NextName))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncFolder(dir)
}

// writeAtomic writes name in dir whole or not at all, readable by its owner alone.
func writeAtomic(dir, name string, b []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, name+"-*") // 0600
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = rename(tmp.Name(), filepath.Join(dir, name))
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return werr
	}
	return syncFolder(dir)
}

// readMoved reads one of the moved clouds over base, with the move that wrote it:
// absent, it is nil. Its errors never quote the secret.
func readMoved(dir, name string, base Cloud) (*Cloud, string, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var f movedFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, "", fmt.Errorf("%s is damaged", name)
	}
	if len(f.URLs) == 0 || f.Secret == "" {
		return nil, "", fmt.Errorf("%s has no urls or no secret", name)
	}
	for i, raw := range f.URLs {
		if err := checkCloudURL(raw); err != nil {
			return nil, "", fmt.Errorf("%s: urls[%d]: %w", name, i, err)
		}
	}
	base.URLs, base.Password = f.URLs, f.Secret
	return &base, f.Ref, nil
}

// syncDir flushes a folder, so that a file renamed into it stays there after a power cut.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}
