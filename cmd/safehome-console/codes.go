package main

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"safehome/internal/access"
	"safehome/internal/adapter/filelock"
	"safehome/internal/adapter/filestore"
	"safehome/internal/consoleapp"
	"safehome/internal/driver"
	"safehome/internal/hub"
	"safehome/internal/hubconfig"
	"safehome/internal/service"
)

// codes fits the hub's access codes to the console's Codes port. Each call reads the
// hub's files again: the contacts may have changed from the app, the codes from the
// command line. A change holds the codes' lock from the read to the write (see
// hubconfig.CodesLock): two phones may set codes at once.
type codes struct {
	hubConfig string
	log       *slog.Logger
}

// codesWait bounds the wait for the codes' lock: a change hashes a few codes, some
// seconds on the hub.
const codesWait = 30 * time.Second

// change runs do on the codes opened under their lock, then tells a running hub.
func (c codes) change(do func(*hub.SiteCodes) error) error {
	cfg, err := hubconfig.Load(c.hubConfig)
	if err != nil {
		return fmt.Errorf("the hub's configuration: %w", err)
	}
	store, err := openStore(cfg) // makes the folder the lock is in
	if err != nil {
		return err
	}
	release, err := filelock.Hold(hubconfig.CodesDir(cfg.StateDir), hubconfig.CodesLock, codesWait)
	if err != nil {
		return err
	}
	defer release()
	site, err := hub.OpenSiteCodes(cfg.Hub, store, driver.SystemClock())
	if err != nil {
		return err
	}
	if err := do(site); err != nil {
		return err
	}
	c.notifyHub(cfg.StateDir)
	return nil
}

func (c codes) open() (*hub.SiteCodes, error) {
	cfg, err := hubconfig.Load(c.hubConfig)
	if err != nil {
		return nil, fmt.Errorf("the hub's configuration: %w", err)
	}
	store, err := openStore(cfg)
	if err != nil {
		return nil, err
	}
	return hub.OpenSiteCodes(cfg.Hub, store, driver.SystemClock())
}

// openStore opens the folder of the hub's access codes. The codes not moved there
// yet (by install.sh) are read where they were, in the state folder itself.
func openStore(cfg hubconfig.Result) (*filestore.Store, error) {
	store, err := filestore.OpenMoved(hubconfig.CodesDir(cfg.StateDir), cfg.StateDir, nil)
	if err != nil {
		return nil, fmt.Errorf("the state directory: %w", err)
	}
	return store, nil
}

// People lists the contacts, and the users of the codes who are not contacts: the
// hub leaves those codes aside.
func (c codes) People() (contacts []consoleapp.Person, leftovers []string, err error) {
	site, err := c.open()
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
// tells a running hub to read the codes again. The code is never in an error. Only a
// contact's code: the hub's codes also hold the installer's, which the console must
// never replace (whoever reaches the page during a mode would take the installation
// mode for himself), and which is set on the hub only (codes set installer).
func (c codes) Set(user, code string) error {
	if user == hub.InstallerUser {
		return fmt.Errorf("%w: %s", consoleapp.ErrNotAContact, user)
	}
	return c.change(func(site *hub.SiteCodes) error {
		err := site.Set(user, code)
		switch {
		case errors.Is(err, hub.ErrNotAContact):
			return fmt.Errorf("%w: %s", consoleapp.ErrNotAContact, user)
		case errors.Is(err, access.ErrInvalidCode):
			return consoleapp.ErrCodeInvalid
		case errors.Is(err, access.ErrWeakCode):
			return consoleapp.ErrCodeWeak
		case errors.Is(err, access.ErrCodeTaken):
			return consoleapp.ErrCodeTaken
		case errors.Is(err, hub.ErrTooManyTaken):
			return consoleapp.ErrCodeWait
		case errors.Is(err, access.ErrTooManyUsers):
			return consoleapp.ErrCodeFull
		}
		return err
	})
}

// Remove removes the code of user, then tells a running hub to read the codes again.
// Never the installer's, which is not the console's (see Set).
func (c codes) Remove(user string) error {
	if user == hub.InstallerUser {
		return consoleapp.ErrNoSuchCode
	}
	return c.change(func(site *hub.SiteCodes) error {
		err := site.Remove(user)
		if errors.Is(err, access.ErrNoSuchUser) {
			return consoleapp.ErrNoSuchCode
		}
		return err
	})
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
