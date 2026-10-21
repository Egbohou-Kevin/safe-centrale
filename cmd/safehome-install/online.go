package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"safehome/internal/adapter/mqtt"
	"safehome/internal/contract"
	"safehome/internal/install"
	"safehome/internal/service"
)

// online connects to the broker, runs fn with a session, and disconnects.
func (c *command) online(ctx context.Context, fn func(context.Context, *install.Session) error) error {
	ids := contract.NewIDGenerator()
	pub := &service.Publisher{}
	s, err := install.NewSession(c.cfg.Hub.Site, install.Deps{Publisher: pub, Now: time.Now, NewID: ids.New}, timing)
	if err != nil {
		return err
	}
	connCtx, stopConn := context.WithCancel(context.Background()) // outlives ctx: Ctrl-C still closes the network
	defer stopConn()
	client, err := mqtt.Dial(connCtx, mqtt.Options{
		BrokerURL: c.cfg.BrokerURL, ClientID: clientID(c.cfg.Hub.Site), Ephemeral: true,
		Subscriptions: service.Subscriptions(s.Subscriptions()),
		OnMessage:     service.Deliver(s.Deliver),
	})
	if err != nil {
		return err
	}
	pub.Attach(client)
	defer func() {
		closeCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		client.Close(closeCtx)
		stop()
	}()
	awaitCtx, stopAwait := context.WithTimeout(ctx, connectWait)
	defer stopAwait()
	if err := client.Await(awaitCtx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("cannot reach the broker at %s: is mosquitto running?", c.cfg.BrokerURL)
	}
	return fn(ctx, s)
}

// clientID is unique for each run: two tools may run at once (watch in one terminal,
// pair in another) without pushing each other off the broker.
func clientID(site string) string {
	var b [4]byte
	rand.Read(b[:])
	return "safehome-install-" + site + "-" + hex.EncodeToString(b[:])
}

func (c *command) snapshot(ctx context.Context, s *install.Session) (install.View, error) {
	snapCtx, stop := context.WithTimeout(ctx, snapshotWait)
	defer stop()
	return s.Snapshot(snapCtx)
}
