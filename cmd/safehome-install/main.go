// Command safehome-install is the installer's tool on the hub (over SSH): it pairs
// and removes devices, gives each one its zone or makes it a siren, checks the site's
// configuration against what is paired, and shows device events live for the tests.
// It is the composition root of the tool: the one place that knows the MQTT client
// and the configuration file.
//
//	safehome-install devices -config /etc/safehome/hub.json
//	safehome-install pair    -config /etc/safehome/hub.json
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
