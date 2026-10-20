// Command safehome-zigbee is the Zigbee adapter that runs on the hub next to
// Zigbee2MQTT: it turns what the sensors report into device events, drives the siren,
// and keeps the device registry through pairing and removal. It is the composition
// root of the adapter: the one place that knows the MQTT client, the disk, the clock
// and where new device ids come from.
//
//	safehome-zigbee run   -config /etc/safehome/zigbee.json
//	safehome-zigbee check -config /etc/safehome/zigbee.json
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// version is set at build time: go build -ldflags "-X main.version=0.1.0".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
