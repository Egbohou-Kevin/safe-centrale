package agentapp

import (
	"strings"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
)

// No log of the agent, at any level, holds the access code or the agent's key: not for
// a seal that opens, nor one given again, nor a code in the clear or malformed. The code
// does reach the hub, so the search below sees it where it is.
func TestNoLogOfTheAgentHoldsTheCodeOrTheKey(t *testing.T) {
	const code = "482915"
	r := newRig(t, nil)
	r.app.LinkChanged(true, "mqtts")
	cmd := topic("alarm", "cmd")
	exp := contract.TSPtr(t0.Add(time.Minute))
	order := contract.AlarmCommandData{Command: contract.CmdDisarm, Args: contract.CommandArgs{Sealed: r.sealed(code, contract.CmdDisarm)},
		Origin: contract.OriginApp, User: "u_owner", Exp: exp}
	r.fromCloud(cmd, contract.TypeAlarmCommand, order)
	r.fromCloud(cmd, contract.TypeAlarmCommand, order) // given again: bad_seal
	r.fromCloud(cmd, contract.TypeAlarmCommand, contract.AlarmCommandData{Command: contract.CmdDisarm,
		Args: contract.CommandArgs{Code: code}, Origin: contract.OriginApp, Exp: exp})
	r.fromCloud(cmd, contract.TypeAlarmCommand, map[string]any{"command": contract.CmdDisarm, "origin": contract.OriginApp,
		"args": map[string]any{"code": 482915}})
	r.fromCloud(cmd, contract.TypeAlarmCommand, contract.AlarmCommandData{Command: contract.CmdSilence,
		Args: contract.CommandArgs{Sealed: r.sealed(code, contract.CmdDisarm)}, Origin: contract.OriginApp, Exp: exp})
	drivertest.WaitFor(t, "the two bad seals answered", func() bool { return len(r.cloud.OfType(contract.TypeAlarmAck)) == 2 })
	r.settle()
	onHub := false
	for _, m := range r.local.On(cmd) {
		onHub = onHub || strings.Contains(string(m.Payload), code)
	}
	if !onHub {
		t.Fatal("the opened code never reached the hub: the search would prove nothing")
	}
	for _, m := range r.cloud.All() {
		if strings.Contains(string(m.Payload), code) {
			t.Errorf("the code went to the cloud on %s", m.Topic)
		}
	}
	logs := r.logs.String()
	if !strings.Contains(logs, "refused a message from the cloud") {
		t.Fatalf("the refusals were not logged: the search would prove nothing\n%s", logs)
	}
	if strings.Contains(logs, code) || strings.Contains(logs, r.key.Encode()) {
		t.Fatalf("a log holds the code or the key:\n%s", logs)
	}
}
