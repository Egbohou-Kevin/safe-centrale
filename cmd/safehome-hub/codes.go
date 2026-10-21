package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"safehome/internal/access"
	"safehome/internal/adapter/filestore"
	"safehome/internal/adapter/pidfile"
	"safehome/internal/hub"
	"safehome/internal/hubconfig"
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
	regs, err := openRegistries(cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	switch args[0] {
	case "list":
		regs.list(stdout)
		return 0
	case "set":
		err = regs.set(args[1], stdin, stdout)
	case "remove":
		err = regs.remove(args[1], stdout)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	notifyRunningHub(cfg.StateDir, stdout)
	return 0
}

// registries are the hub's two code registries: the people's, and the installer's,
// with the site's contacts: the hub leaves aside the code of anyone else.
type registries struct {
	people, installer *access.Registry
	contacts          map[string]bool
}

func openRegistries(cfg hubconfig.Result) (registries, error) {
	store, err := filestore.Open(cfg.StateDir, nil)
	if err != nil {
		return registries{}, fmt.Errorf("cannot open the state directory: %w", err)
	}
	people, err := hub.OpenCodes(cfg.Hub.Access, store)
	if err != nil {
		return registries{}, fmt.Errorf("cannot read the access codes: %w", err)
	}
	installer, err := hub.OpenInstallerCode(cfg.Hub.Access, store)
	if err != nil {
		return registries{}, fmt.Errorf("cannot read the installer code: %w", err)
	}
	contacts := map[string]bool{}
	for _, c := range cfg.Hub.Escalation.Contacts {
		contacts[c.ID] = true
	}
	return registries{people: people, installer: installer, contacts: contacts}, nil
}

// of returns the registry of user, and the other one, whose codes the user's must
// not match: one code opens one thing.
func (r registries) of(user string) (own, other *access.Registry) {
	if user == hub.InstallerUser {
		return r.installer, r.people
	}
	return r.people, r.installer
}

func (r registries) list(stdout io.Writer) {
	people, installer := r.people.Users(), r.installer.Users()
	if len(people)+len(installer) == 0 {
		fmt.Fprintln(stdout, "no access code is set")
	}
	for _, u := range people {
		if r.contacts[u] {
			fmt.Fprintln(stdout, u)
		} else {
			fmt.Fprintf(stdout, "%s (not a contact of the site: the hub leaves this code aside, remove it)\n", u)
		}
	}
	for _, u := range installer {
		fmt.Fprintln(stdout, u)
	}
}

// set reads the code on in and gives it to user, a contact of the site or the
// installer.
func (r registries) set(user string, in io.Reader, stdout io.Writer) error {
	if user != hub.InstallerUser && !r.contacts[user] {
		return fmt.Errorf("the code was not set: %s is not a contact of the site (see the configuration), the hub would leave it aside", user)
	}
	code, err := readCode(in)
	if err != nil {
		return err
	}
	own, other := r.of(user)
	if _, taken := other.Verify(code); taken {
		err = access.ErrCodeTaken
	} else {
		err = own.Set(user, code)
	}
	if err != nil {
		return fmt.Errorf("the code was not set: %s", describe(err))
	}
	fmt.Fprintf(stdout, "code set for %s\n", user)
	return nil
}

func (r registries) remove(user string, stdout io.Writer) error {
	own, _ := r.of(user)
	if err := own.Remove(user); err != nil {
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
	pid, ok := pidfile.Read(dir, pidName)
	if !ok || !pidfile.Alive(pid) {
		fmt.Fprintln(stdout, "no hub is running: the new codes apply at its next start")
		return
	}
	p, err := os.FindProcess(pid)
	if err == nil && p.Signal(syscall.SIGHUP) == nil {
		fmt.Fprintf(stdout, "the running hub (process %d) was told to reload the codes\n", pid)
		return
	}
	fmt.Fprintf(stdout, "could not signal process %d: restart the hub to apply the codes\n", pid)
}
