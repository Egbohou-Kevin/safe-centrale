package main

import (
	"context"
	"fmt"
	"io"

	"safehome/internal/adapter/mqtt"
	"safehome/internal/adapter/x728"
	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/powerapp"
	"safehome/internal/powerconfig"
	"safehome/internal/service"
)

const (
	name    = "battery module service" // in the logs
	pidName = "power.pid"
)

var commandLine = service.CLI{Program: "safehome-power", Version: version, Usage: `usage:
  safehome-power run   -config FILE   run the battery module service
  safehome-power check -config FILE   check the configuration, read the module once, and exit
  safehome-power version
`}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cmd, exit, ok := commandLine.Parse(args, stdout, stderr)
	if !ok {
		return exit
	}
	cfg, err := powerconfig.Load(cmd.Config)
	if err != nil {
		fmt.Fprintln(stderr, "configuration error:", err)
		return 1
	}
	if cmd.Name == "check" {
		return check(ctx, cfg, stdout, stderr)
	}
	return runService(ctx, cfg, stderr)
}

func openBoard(b powerconfig.Board) (*x728.Board, error) {
	return x728.Open(x728.Options{GPIOChip: b.GPIOChip, MainsLine: b.MainsLine, CutLine: b.CutLine, CutPulse: b.CutPulse, I2CBus: b.I2CBus})
}

// check says what the service would do, then reads the module once. A module that
// does not answer is said but does not fail the check: the installation may come
// before the reboot that turns the I2C bus on (docs/alimentation.md).
func check(ctx context.Context, cfg powerconfig.Result, stdout, stderr io.Writer) int {
	b := cfg.Board
	fmt.Fprintf(stdout, "configuration OK: site %s, device %s, mains on line %d and power button on line %d of %s, gauge on %s, shut down below %d mV\n",
		cfg.App.Site, cfg.App.DeviceID, b.MainsLine, b.CutLine, b.GPIOChip, b.I2CBus, cfg.App.Power.CutMilliVolts)
	board, err := openBoard(b)
	if err != nil {
		fmt.Fprintln(stderr, "the battery module cannot be reached now (see docs/alimentation.md):", err)
		return 0
	}
	defer board.Close()
	lost, lineErr := board.MainsLost(ctx)
	mv, percent, gaugeErr := board.Gauge(ctx)
	if lineErr != nil || gaugeErr != nil {
		fmt.Fprintln(stderr, "the battery module does not answer:", lineErr, gaugeErr)
		return 0
	}
	fmt.Fprintf(stdout, "battery module: mains lost %v, cell %d mV, charge %d %%\n", lost, mv, percent)
	return 0
}

func runService(ctx context.Context, cfg powerconfig.Result, stderr io.Writer) int {
	log := service.Logger(stderr, cfg.LogLevel)
	store, release, err := service.OpenState(name, cfg.StateDir, pidName, log)
	if err != nil {
		log.Error("cannot start", "err", err)
		return 1
	}
	defer release()

	board, err := openBoard(cfg.Board)
	if err != nil {
		log.Error("cannot reach the battery module", "err", err)
		return 1
	}
	defer board.Close()
	ids := contract.NewIDGenerator()
	pub := &service.Publisher{}
	app, err := powerapp.New(cfg.App, powerapp.Deps{
		Clock: driver.SystemClock(), Store: store, Publisher: pub, Board: board, NewID: ids.New, Logger: log,
	})
	if err != nil {
		log.Error("cannot start the battery module service", "err", err)
		return 1
	}
	log.Info("starting", "site", cfg.App.Site, "device", cfg.App.DeviceID, "broker", cfg.BrokerURL, "version", version)

	return service.Daemon{
		Name: name, Log: log, Loops: []func(context.Context) error{app.Run}, Alive: app.Alive,
		Brokers: []service.Broker{{Publisher: pub, Options: mqtt.Options{BrokerURL: cfg.BrokerURL, ClientID: cfg.ClientID, Logger: log}}},
	}.Run(ctx)
}
