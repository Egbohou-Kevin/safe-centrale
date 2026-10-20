// Command safehome-gsm is the GSM gateway that runs on the hub next to
// safehome-hub: it owns the modems, sends the SMS and calls the escalation asks for,
// and turns the contacts' "1" answers into acknowledgements. It is the composition
// root of the gateway: the one place that knows the real modems, the MQTT client,
// the disk and the clock.
//
//	safehome-gsm run   -config /etc/safehome/gsm.json
//	safehome-gsm check -config /etc/safehome/gsm.json
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
