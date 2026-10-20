// Command safehome-agent is the link agent on the hub: it carries to the cloud what
// the contract lets go up (alarms, alerts, states, health), keeps it on disk while
// the internet is down, and publishes on the hub only what the cloud is allowed to
// send. It is the composition root of the agent: the one place that knows the two
// MQTT clients, the disk and the clock.
//
//	safehome-agent run   -config /etc/safehome/agent.json
//	safehome-agent check -config /etc/safehome/agent.json
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
