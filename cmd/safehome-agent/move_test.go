package main

import (
	"slices"
	"testing"

	"safehome/internal/agentconfig"
	"safehome/internal/service"
)

// A move under way connects to its new cell; otherwise the current one.
func TestTheAgentConnectsToTheCellOfAMoveUnderWay(t *testing.T) {
	a := agentconfig.Cloud{URLs: []string{"mqtts://a.example:8883"}, Password: "pw-a"}
	b := agentconfig.Cloud{URLs: []string{"mqtts://b.example:8883"}, Password: "pw-b"}
	if got := cloudOf(agentconfig.Result{Cloud: a}); !slices.Equal(got.URLs, a.URLs) || got.Password != "pw-a" {
		t.Fatalf("%v", got)
	}
	if got := cloudOf(agentconfig.Result{Cloud: a, Next: &b}); !slices.Equal(got.URLs, b.URLs) || got.Password != "pw-b" {
		t.Fatalf("%v", got)
	}
}

// The mover writes in the agent's folder and restarts the agent to connect.
func TestTheMoverWritesThenRestarts(t *testing.T) {
	dir := t.TempDir()
	restart := service.NewRestart()
	m := mover{dir: dir, restart: restart}
	if err := m.Write([]string{"mqtts://b.example:8883"}, "s3cret-for-cell-b-0001"); err != nil {
		t.Fatal(err)
	}
	m.Reconnect()
	select {
	case <-restart:
	default:
		t.Fatal("no restart asked")
	}
	if err := m.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := m.Discard(); err != nil {
		t.Fatal(err)
	}
}
