// Command safehome-console is the hub's local console: while the installation mode
// lasts (opened with the installer code at the keypad), it turns on the hub's own
// Wi-Fi network and serves a web page there, for the installer's phone: pairing,
// zones, sirens, tests. It is the composition root of the console: the one place that
// knows the MQTT client, NetworkManager, the web server, the files and the clock.
//
//	safehome-console run   -config /etc/safehome/console.json
//	safehome-console check -config /etc/safehome/console.json
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
