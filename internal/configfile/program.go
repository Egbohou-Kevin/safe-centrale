package configfile

import (
	"cmp"
	"fmt"
	"log/slog"
)

// ProgramFile is the head every program's configuration file shares. A configuration
// package embeds it in the struct it decodes: the keys stay at the top of the file.
type ProgramFile struct {
	Site      string `json:"site"`
	BrokerURL string `json:"broker_url"`
	ClientID  string `json:"client_id"`
	StateDir  string `json:"state_dir"`
	LogLevel  string `json:"log_level"`
}

// Program is what ProgramFile gives a program, defaults applied. A configuration
// package embeds it in its result.
type Program struct {
	BrokerURL string // the local broker
	ClientID  string
	StateDir  string
	LogLevel  slog.Level
}

// Build applies the defaults: the local broker, and a client id made of clientPrefix
// and the site ("safehome-gsm-" for the GSM gateway). The state folder is required;
// stateUse says what the program keeps there, for the error that asks for it.
func (f ProgramFile) Build(clientPrefix, stateUse string) (Program, error) {
	if f.StateDir == "" {
		return Program{}, fmt.Errorf("state_dir is required (%s)", stateUse)
	}
	level, err := Level(f.LogLevel)
	if err != nil {
		return Program{}, err
	}
	return Program{
		BrokerURL: cmp.Or(f.BrokerURL, "mqtt://127.0.0.1:1883"),
		ClientID:  cmp.Or(f.ClientID, clientPrefix+f.Site),
		StateDir:  f.StateDir,
		LogLevel:  level,
	}, nil
}
