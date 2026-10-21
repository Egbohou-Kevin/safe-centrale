package consoleapp

import (
	"fmt"
	"slices"
)

// SetCode sets the access code of a contact of the site, typed twice by that person
// on the spot. Codes.Set refuses someone who is not a contact. The code is never
// logged nor kept here.
func (a *App) SetCode(user, code, again string) error {
	if _, err := a.sessionCtx(); err != nil {
		return err
	}
	if code != again {
		return ErrCodeMismatch
	}
	if err := a.deps.Codes.Set(user, code); err != nil {
		return err
	}
	a.log.Info("access code set from the console", "user", user)
	return nil
}

// RemoveCode removes the code of someone who is no longer a contact of the site: the
// hub leaves it aside, but it still takes one of the places.
func (a *App) RemoveCode(user string) error {
	if _, err := a.sessionCtx(); err != nil {
		return err
	}
	_, leftovers, err := a.deps.Codes.People()
	if err != nil {
		return err
	}
	if !slices.Contains(leftovers, user) {
		return fmt.Errorf("%w: %s", ErrNoSuchCode, user)
	}
	if err := a.deps.Codes.Remove(user); err != nil {
		return err
	}
	a.log.Info("access code of a former contact removed from the console", "user", user)
	return nil
}
