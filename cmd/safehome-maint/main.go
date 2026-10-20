// Command safehome-maint is the hub's remote maintenance service: when the cloud asks
// for it, it opens an SSH tunnel out to our bastion for an hour at most, and reports
// every step, which the owner sees. It is the composition root of the service: the
// one place that knows the MQTT client, ssh, the disk and the clock.
//
//	safehome-maint run   -config /etc/safehome/maint.json
//	safehome-maint check -config /etc/safehome/maint.json
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
