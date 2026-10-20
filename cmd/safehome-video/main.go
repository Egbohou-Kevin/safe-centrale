// Command safehome-video is the site's video service: when the cloud asks for a live
// session, it has go2rtc push the camera's stream to the cloud relay, keeps to the
// month's allowance on mobile data, and reports how the session goes. It is the
// composition root of the service: the one place that knows the MQTT client, go2rtc,
// the disk and the clock.
//
//	safehome-video run   -config /etc/safehome/video.json
//	safehome-video check -config /etc/safehome/video.json
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
