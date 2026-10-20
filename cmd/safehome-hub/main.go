// Command safehome-hub is the program that runs on the hub (the Raspberry Pi). It
// is the composition root: the one place that knows the real MQTT client, the real
// disk and the real clock, and wires them to the application (package hub).
//
//	safehome-hub run   -config /etc/safehome/hub.json
//	safehome-hub check -config /etc/safehome/hub.json
//	safehome-hub codes -config /etc/safehome/hub.json list
//	safehome-hub codes -config /etc/safehome/hub.json set u_owner < code.txt
//	safehome-hub codes -config /etc/safehome/hub.json remove u_owner
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
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
