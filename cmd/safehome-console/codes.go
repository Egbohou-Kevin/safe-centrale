package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"

	"safehome/internal/access"
	"safehome/internal/adapter/filestore"
	"safehome/internal/adapter/pidfile"
	"safehome/internal/consoleapp"
	"safehome/internal/hub"
	"safehome/internal/hubconfig"
)

// hubPidName is the hub's pid file in the state directory: the hub reads the codes
// again on SIGHUP, as after "safehome-hub codes set".
const hubPidName = "hub.pid"

// codes fits the hub's access codes to the console's Codes port. Each call reads the
// hub's files again: the contacts may have changed from the app, the codes from the
// command line.
type codes struct {
	hubConfig string
	log       *slog.Logger
}

type opened struct {
	cfg               hubconfig.Result
	people, installer *access.Registry
}

func (c codes) open() (opened, error) {
	cfg, err := hubconfig.Load(c.hubConfig)
	if err != nil {
		return opened{}, fmt.Errorf("the hub's configuration: %w", err)
	}
	store, err := filestore.Open(cfg.StateDir, nil)
	if err != nil {
		return opened{}, fmt.Errorf("the state directory: %w", err)
	}
	people, err := hub.OpenCodes(cfg.Hub.Access, store)
	if err != nil {
		return opened{}, err
	}
	installer, err := hub.OpenInstallerCode(cfg.Hub.Access, store)
	if err != nil {
		return opened{}, err
	}
	return opened{cfg: cfg, people: people, installer: installer}, nil
}

func (c codes) People() ([]consoleapp.Person, error) {
	o, err := c.open()
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, u := range o.people.Users() {
		have[u] = true
	}
	var out []consoleapp.Person
	for _, ct := range o.cfg.Hub.Escalation.Contacts {
		out = append(out, consoleapp.Person{ID: ct.ID, Name: ct.Name, Role: string(ct.Role), HasCode: have[ct.ID]})
	}
	return out, nil
}

// Set saves the code, unless it is the installer's (one code opens one thing), then
// tells a running hub to read the codes again. The code is never in an error.
func (c codes) Set(user, code string) error {
	o, err := c.open()
	if err != nil {
		return err
	}
	if _, taken := o.installer.Verify(code); taken {
		return consoleapp.ErrCodeTaken
	}
	if err := o.people.Set(user, code); err != nil {
		switch {
		case errors.Is(err, access.ErrInvalidCode):
			return consoleapp.ErrCodeInvalid
		case errors.Is(err, access.ErrWeakCode):
			return consoleapp.ErrCodeWeak
		case errors.Is(err, access.ErrCodeTaken):
			return consoleapp.ErrCodeTaken
		case errors.Is(err, access.ErrTooManyUsers):
			return consoleapp.ErrCodeFull
		}
		return err
	}
	c.notifyHub(o.cfg.StateDir)
	return nil
}

func (c codes) notifyHub(dir string) {
	pid, ok := pidfile.Read(dir, hubPidName)
	if !ok || !pidfile.Alive(pid) {
		c.log.Warn("no hub is running: the new code applies at its next start")
		return
	}
	p, err := os.FindProcess(pid)
	if err == nil {
		err = p.Signal(syscall.SIGHUP)
	}
	if err != nil {
		c.log.Error("could not tell the hub to read the codes again: restart it", "pid", pid, "err", err)
		return
	}
	c.log.Info("the hub was told to read the codes again", "pid", pid)
}
