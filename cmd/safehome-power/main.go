// Command safehome-power is the hub's battery module service: it reads the X728's
// mains line and fuel gauge, tells the site the hub's supply and charge, and has the
// module shut the hub down before its battery is empty. It is the composition root of
// the service: the one place that knows the MQTT client, the module, the disk and the
// clock.
//
//	safehome-power run   -config /etc/safehome/power.json
//	safehome-power check -config /etc/safehome/power.json
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
