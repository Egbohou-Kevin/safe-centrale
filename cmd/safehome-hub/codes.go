package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"safehome/internal/access"
	"safehome/internal/adapter/filelock"
	"safehome/internal/adapter/filestore"
	"safehome/internal/driver"
	"safehome/internal/hub"
	"safehome/internal/hubconfig"
	"safehome/internal/service"
)

// codes runs "safehome-hub codes": it lists, sets or removes the access codes, then
// tells a running hub to read them again.
func codes(cfg hubconfig.Result, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch {
	case args[0] != "list" && args[0] != "set" && args[0] != "remove":
		fmt.Fprintf(stderr, "unknown codes command %q\n%s", args[0], usage)
		return 2
	case args[0] != "list" && len(args) != 2:
		fmt.Fprint(stderr, usage)
		return 2
	}
	var code string
	if args[0] == "set" { // read before the lock is held: whoever types takes their time
		var err error
		if code, err = readCode(stdin); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	site, release, err := openCodes(cfg, args[0] != "list")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer release()
	switch args[0] {
	case "list":
		list(site.List(), stdout)
		return 0
	case "set":
		err = set(site, args[1], code, stdout)
	case "remove":
		err = remove(site, args[1], stdout)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	notifyRunningHub(cfg.StateDir, stdout)
	return 0
}

// codesWait bounds the wait for the codes' lock, held by the local console while it
// saves a code (a few seconds).
const codesWait = 30 * time.Second

// openCodes reads the access codes; to change them, under their lock (see
// hubconfig.CodesLock), held from the read to the write until release.
func openCodes(cfg hubconfig.Result, change bool) (*hub.SiteCodes, func(), error) {
	store, err := openCodesStore(cfg.StateDir, nil) // makes the folder the lock is in
	if err != nil {
		return nil, nil, fmt.Errorf("cannot open the state directory: %w", err)
	}
	release := func() {}
	if change {
		if release, err = filelock.Hold(hubconfig.CodesDir(cfg.StateDir), hubconfig.CodesLock, codesWait); err != nil {
			return nil, nil, fmt.Errorf("the access codes are being changed elsewhere: %w", err)
		}
	}
	site, err := hub.OpenSiteCodes(cfg.Hub, store, driver.SystemClock())
	if err != nil {
		release()
		return nil, nil, fmt.Errorf("cannot read the access codes: %w", err)
	}
	return site, release, nil
}

// openCodesStore opens the folder of the access codes in the state folder dir. The
// codes not moved there yet (by install.sh) are read where they were, in dir itself.
func openCodesStore(dir string, log *slog.Logger) (*filestore.Store, error) {
	return filestore.OpenMoved(hubconfig.CodesDir(dir), dir, log)
}

func list(l hub.CodeList, stdout io.Writer) {
	empty := len(l.Leftovers) == 0 && !l.Installer
	for _, c := range l.Contacts {
		if c.HasCode {
			fmt.Fprintln(stdout, c.ID)
			empty = false
		}
	}
	for _, u := range l.Leftovers {
		fmt.Fprintf(stdout, "%s (not a contact of the site: the hub leaves this code aside, remove it)\n", u)
	}
	if l.Installer {
		fmt.Fprintln(stdout, hub.InstallerUser)
	}
	if empty {
		fmt.Fprintln(stdout, "no access code is set")
	}
}

// set gives the code read on the standard input to user, a contact of the site or
// the installer.
func set(site *hub.SiteCodes, user, code string, stdout io.Writer) error {
	if err := site.Set(user, code); errors.Is(err, hub.ErrNotAContact) {
		return fmt.Errorf("the code was not set: %s is not a contact of the site (see the configuration), the hub would leave it aside", user)
	} else if err != nil {
		return fmt.Errorf("the code was not set: %s", describe(err))
	}
	fmt.Fprintf(stdout, "code set for %s\n", user)
	return nil
}

func remove(site *hub.SiteCodes, user string, stdout io.Writer) error {
	if err := site.Remove(user); err != nil {
		return fmt.Errorf("nothing was removed: %s", describe(err))
	}
	fmt.Fprintf(stdout, "code removed for %s\n", user)
	return nil
}

// readCode reads the first line of the input. It is never echoed back, and never
// part of a message from this program.
func readCode(in io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(in, 256)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("reading the code: %w", err)
	}
	code := strings.TrimSpace(line)
	if code == "" {
		return "", errors.New("no code given: write it on the standard input")
	}
	return code, nil
}

func describe(err error) string {
	switch {
	case errors.Is(err, access.ErrInvalidCode):
		return "a code is 4 to 8 digits"
	case errors.Is(err, access.ErrWeakCode):
		return "too easy to guess (no repeated digit, no straight run of digits)"
	case errors.Is(err, access.ErrCodeTaken):
		return "another person already has this code"
	case errors.Is(err, hub.ErrTooManyTaken):
		return "too many codes refused as another person's: wait 10 minutes"
	case errors.Is(err, access.ErrTooManyUsers):
		return "the site already has the maximum number of codes: remove one first"
	case errors.Is(err, access.ErrInvalidUser):
		return "invalid user identifier"
	case errors.Is(err, access.ErrNoSuchUser):
		return "no such user"
	}
	return err.Error()
}

// notifyRunningHub asks a running hub to read the codes again.
func notifyRunningHub(dir string, stdout io.Writer) {
	switch pid, err := service.Reload(dir, pidName); {
	case errors.Is(err, service.ErrNotRunning):
		fmt.Fprintln(stdout, "no hub is running: the new codes apply at its next start")
	case err != nil:
		fmt.Fprintf(stdout, "could not signal process %d: restart the hub to apply the codes\n", pid)
	default:
		fmt.Fprintf(stdout, "the running hub (process %d) was told to reload the codes\n", pid)
	}
}
