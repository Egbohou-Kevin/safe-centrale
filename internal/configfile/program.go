package configfile

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	Username  string   // the program's name on the local broker: "safehome-gsm"
	Password  Password // its password there, from systemd's credentials; empty without them
	StateDir  string
	LogLevel  slog.Level
}

// The local broker refuses a client without its name and password (docs/systeme.md,
// "Mosquitto"). systemd gives each unit its own password (LoadCredential=) in the
// folder CREDENTIALS_DIRECTORY names, readable by that unit alone.
const (
	credentialsEnv = "CREDENTIALS_DIRECTORY"
	// PasswordCredential is the name of the password among the unit's credentials.
	PasswordCredential = "mqtt-password"
	// OperatorSecret is the operator's password, readable by root only: the tools run
	// by hand under sudo (safehome-install) read it.
	OperatorSecret = "/etc/safehome/mqtt/operator.secret"
	operatorName   = "operator"
)

// Password is a password on the local broker. It never shows in a log, an error or a
// printed configuration: Reveal is the only way to its value.
type Password string

// Reveal returns the password itself, to hand to the MQTT client and nothing else.
func (p Password) Reveal() string { return string(p) }

// Format shows whether there is a password, never which.
func (p Password) Format(f fmt.State, _ rune) { io.WriteString(f, p.shown()) }

// LogValue keeps the password out of the logs.
func (p Password) LogValue() slog.Value { return slog.StringValue(p.shown()) }

// MarshalText keeps the password out of an encoded configuration.
func (p Password) MarshalText() ([]byte, error) { return []byte(p.shown()), nil }

func (p Password) shown() string {
	if p == "" {
		return ""
	}
	return "***"
}

// Build applies the defaults: the local broker, and a client id made of clientPrefix
// and the site ("safehome-gsm-" for the GSM gateway). The program's name on the broker
// is clientPrefix without its last dash, its password the one systemd gives it. The
// state folder is required; stateUse says what the program keeps there, for the error
// that asks for it.
func (f ProgramFile) Build(clientPrefix, stateUse string) (Program, error) {
	if f.StateDir == "" {
		return Program{}, fmt.Errorf("state_dir is required (%s)", stateUse)
	}
	level, err := Level(f.LogLevel)
	if err != nil {
		return Program{}, err
	}
	password, err := unitPassword()
	if err != nil {
		return Program{}, err
	}
	return Program{
		BrokerURL: cmp.Or(f.BrokerURL, "mqtt://127.0.0.1:1883"),
		ClientID:  cmp.Or(f.ClientID, clientPrefix+f.Site),
		Username:  strings.TrimSuffix(clientPrefix, "-"),
		Password:  password,
		StateDir:  f.StateDir,
		LogLevel:  level,
	}, nil
}

// LocalBroker returns what a client of the local broker needs: its address, the client
// id, and the program's name and password there.
func (p Program) LocalBroker() (url, clientID, username, password string) {
	return p.BrokerURL, p.ClientID, p.Username, p.Password.Reveal()
}

// AsOperator returns p as the operator on the local broker, for a tool run by hand
// under sudo: the operator's name and the password in OperatorSecret. Without that
// file (a computer that is not a hub), there is no password.
func (p Program) AsOperator() (Program, error) { return p.asOperator(OperatorSecret) }

func (p Program) asOperator(path string) (Program, error) {
	password, err := readPassword(path, true)
	if err != nil {
		return Program{}, err
	}
	p.Username, p.Password = operatorName, password
	return p, nil
}

// unitPassword reads the password systemd gives the unit. Outside systemd (a trial, a
// Mac) there is none: the hub's broker would refuse the program, a test broker takes it.
func unitPassword() (Password, error) {
	dir := os.Getenv(credentialsEnv)
	if dir == "" {
		return "", nil
	}
	return readPassword(filepath.Join(dir, PasswordCredential), false)
}

// readPassword reads a password file. An error names the file, never what it holds;
// a missing file gives no password when missingOK.
func readPassword(path string, missingOK bool) (Password, error) {
	b, err := os.ReadFile(path)
	if missingOK && errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			err = pathErr.Err
		}
		return "", fmt.Errorf("cannot read the broker password %s: %w", path, err)
	}
	password := strings.TrimSpace(string(b))
	if password == "" {
		return "", fmt.Errorf("the broker password %s is empty", path)
	}
	return Password(password), nil
}
