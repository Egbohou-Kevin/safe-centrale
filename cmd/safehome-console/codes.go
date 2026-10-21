package main

import (
	"errors"
	"fmt"
	"log/slog"

	"safehome/internal/access"
	"safehome/internal/adapter/filestore"
	"safehome/internal/consoleapp"
	"safehome/internal/hub"
	"safehome/internal/hubconfig"
	"safehome/internal/service"
)

// codes fits the hub's access codes to the console's Codes port. Each call reads the
// hub's files again: the contacts may have changed from the app, the codes from the
// command line.
type codes struct {
	hubConfig string
	log       *slog.Logger
}

func (c codes) open() (*hub.SiteCodes, string, error) {
	cfg, err := hubconfig.Load(c.hubConfig)
	if err != nil {
		return nil, "", fmt.Errorf("the hub's configuration: %w", err)
	}
	store, err := filestore.Open(cfg.StateDir, nil)
	if err != nil {
		return nil, "", fmt.Errorf("the state directory: %w", err)
	}
	site, err := hub.OpenSiteCodes(cfg.Hub, store)
	return site, cfg.StateDir, err
}

// People lists the contacts, and the users of the codes who are not contacts: the
// hub leaves those codes aside.
func (c codes) People() (contacts []consoleapp.Person, leftovers []string, err error) {
	site, _, err := c.open()
	if err != nil {
		return nil, nil, err
	}
	l := site.List()
	for _, ct := range l.Contacts {
		contacts = append(contacts, consoleapp.Person{ID: ct.ID, Name: ct.Name, Role: string(ct.Role), HasCode: ct.HasCode})
	}
	return contacts, l.Leftovers, nil
}

// Set saves the code, unless it is the installer's (one code opens one thing), then
// tells a running hub to read the codes again. The code is never in an error.
func (c codes) Set(user, code string) error {
	site, dir, err := c.open()
	if err != nil {
		return err
	}
	if err := site.Set(user, code); err != nil {
		switch {
		case errors.Is(err, hub.ErrNotAContact):
			return fmt.Errorf("%w: %s", consoleapp.ErrNotAContact, user)
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
	c.notifyHub(dir)
	return nil
}

// Remove removes the code of user, then tells a running hub to read the codes again.
func (c codes) Remove(user string) error {
	site, dir, err := c.open()
	if err != nil {
		return err
	}
	if err := site.Remove(user); err != nil {
		if errors.Is(err, access.ErrNoSuchUser) {
			return consoleapp.ErrNoSuchCode
		}
		return err
	}
	c.notifyHub(dir)
	return nil
}

func (c codes) notifyHub(dir string) {
	switch pid, err := service.Reload(dir, hubconfig.PidFile); {
	case errors.Is(err, service.ErrNotRunning):
		c.log.Warn("no hub is running: the new code applies at its next start")
	case err != nil:
		c.log.Error("could not tell the hub to read the codes again: restart it", "pid", pid, "err", err)
	default:
		c.log.Info("the hub was told to read the codes again", "pid", pid)
	}
}
