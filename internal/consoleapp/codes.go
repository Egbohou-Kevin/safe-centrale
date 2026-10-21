package consoleapp

import (
	"fmt"
	"slices"
)

// SetCode sets the access code of a contact of the site, typed twice by that person
// on the spot. The code is never logged nor kept here.
func (a *App) SetCode(user, code, again string) error {
	if _, err := a.sessionCtx(); err != nil {
		return err
	}
	if code != again {
		return ErrCodeMismatch
	}
	people, err := a.deps.Codes.People()
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(people, func(p Person) bool { return p.ID == user }) {
		return fmt.Errorf("%w: %s", ErrNotAContact, user)
	}
	if err := a.deps.Codes.Set(user, code); err != nil {
		return err
	}
	a.log.Info("access code set from the console", "user", user)
	return nil
}
