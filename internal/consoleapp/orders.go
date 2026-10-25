package consoleapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"safehome/internal/contract"
	"safehome/internal/install"
)

// sessionCtx returns the current installation mode's context, or ErrClosed.
func (a *App) sessionCtx() (context.Context, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.open || a.session == nil {
		return nil, ErrClosed
	}
	return a.session, nil
}

// submit hands an order to the worker; only one at a time.
func (a *App) submit(job func(context.Context)) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy {
		return ErrBusy
	}
	done := func(ctx context.Context) {
		defer func() { a.mu.Lock(); a.busy = false; a.mu.Unlock() }()
		job(ctx)
	}
	select {
	case a.jobs <- done:
		a.busy = true
		return nil
	default:
		return ErrBusy
	}
}

// Pair opens the Zigbee network for PairFor. It returns at once; the page shows the
// devices as they join.
func (a *App) Pair() error {
	sctx, err := a.sessionCtx()
	if err != nil {
		return err
	}
	return a.submit(func(context.Context) { a.pair(sctx) })
}

// pair runs one pairing window of the mode whose context is sctx. The window keeps
// its own record: if that mode ends and another opens meanwhile, the new mode's
// page never shows it.
func (a *App) pair(sctx context.Context) {
	p := &Pairing{Active: true, Until: a.deps.Clock.Now().Add(a.cfg.PairFor)}
	a.mu.Lock()
	current := a.session == sctx
	if current {
		a.pairing = p
	}
	a.mu.Unlock()
	if !current {
		return // the mode ended before the order's turn came
	}
	a.log.Info("pairing opened from the console", "for", a.cfg.PairFor)
	err := a.sess.Pair(sctx, a.cfg.PairFor, func(n install.PairNews) {
		a.mu.Lock()
		defer a.mu.Unlock()
		switch {
		case n.Added != nil:
			p.Added = append(p.Added, *n.Added)
		case n.Rejected != nil:
			p.Rejected = append(p.Rejected, *n.Rejected)
		}
	})
	a.mu.Lock()
	p.Active = false
	if err != nil {
		p.Err = err.Error()
	}
	added := len(p.Added)
	a.mu.Unlock()
	a.log.Info("pairing over", "added", added, "err", err)
}

// Remove takes a device out of the network for good, then out of the site. It is
// refused before anything is sent if the site without it would not work (the last
// siren). It waits for the Zigbee adapter's answer; once the order is sent, the
// removal and the site's change go to their end even if the mode ends meanwhile, so
// the site never keeps a device the network lost.
func (a *App) Remove(ctx context.Context, id string) error {
	sctx, err := a.sessionCtx()
	if err != nil {
		return err
	}
	if _, err := a.paired(id); err != nil {
		return err
	}
	if _, err := a.site.CheckRemoval(id); err != nil {
		return err
	}
	result := make(chan error, 1)
	if err := a.submit(func(wctx context.Context) { result <- a.remove(sctx, wctx, id) }); err != nil {
		a.site.CancelRemoval(id)
		return err
	}
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-sctx.Done():
		return ErrClosed // the console may stop before the order's turn comes
	}
}

// remove runs on the worker: it removes the device from the network, unless the mode
// whose context is sctx ended before the order's turn came, then from the site. The
// wait for the adapter is bounded by the session, and by wctx, the console's life. An
// order that went out without an answer may still be carried out a moment later: the
// site stops using the device all the same, and the installer is told.
func (a *App) remove(sctx, wctx context.Context, id string) error {
	if sctx.Err() != nil {
		a.site.CancelRemoval(id)
		return ErrClosed
	}
	err := a.sess.Remove(wctx, id)
	switch {
	case err == nil:
		a.log.Info("device removed from the console", "device", id)
	case errors.Is(err, install.ErrUnconfirmed):
		a.log.Warn("removal not confirmed: the device is taken out of the site all the same", "device", id, "err", err)
	default:
		a.site.CancelRemoval(id)
		return err
	}
	_, ferr := a.site.FinishRemoval(id)
	return errors.Join(err, ferr)
}

// Finish asks the hub to close the installation mode: the console stops when the hub
// says it is closed, and the hub restarts if the zones or the sirens changed.
func (a *App) Finish(ctx context.Context) error {
	if _, err := a.sessionCtx(); err != nil {
		return err
	}
	now := a.deps.Clock.Now()
	env, err := contract.NewEnvelope(a.cfg.Site, Source, uint64(now.UnixMilli()), a.deps.NewID(), now, contract.TypeInstallClose, contract.InstallCloseData{})
	if err != nil {
		return err
	}
	payload, err := json.Marshal(env)
	if err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, a.cfg.Wait)
	defer cancel()
	if err := a.deps.Publisher.Publish(pctx, contract.InstallCmdTopic(a.cfg.Site), payload, 1, false); err != nil {
		return fmt.Errorf("the hub could not be told: %w", err)
	}
	a.log.Info("the installer closed the installation mode from the console")
	return nil
}
