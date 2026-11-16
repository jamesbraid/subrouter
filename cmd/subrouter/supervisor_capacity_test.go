package main

import (
	"os"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/front"
)

func TestSupervisorCapacityConfiguration(t *testing.T) {
	for _, value := range []string{"0", "2", "3"} {
		config, err := parseSupervisorConfig([]string{"--worker-bin", os.Args[0], "--max-generations", value})
		if err != nil {
			t.Fatal(err)
		}
		if err := validateSupervisorConfig(config); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{"-1", "1"} {
		config, err := parseSupervisorConfig([]string{"--worker-bin", os.Args[0], "--max-generations", value})
		if err != nil {
			t.Fatal(err)
		}
		if err := validateSupervisorConfig(config); err == nil {
			t.Fatalf("invalid generation cap %s was accepted", value)
		}
	}
}

func TestSupervisorRefusesNewGenerationWithoutInterruptingOldStreams(t *testing.T) {
	router, err := front.NewRouter(front.Backend{ID: "active", Network: "unix", Address: "/fixture/active.sock"})
	if err != nil {
		t.Fatal(err)
	}
	s := &supervisor{
		config: supervisorConfig{MaxGenerations: 2, WorkerBin: "/must-not-start"}, router: router,
		workers: map[string]*workerGeneration{
			"active":  {id: "active", done: make(chan struct{})},
			"retired": {id: "retired", done: make(chan struct{})},
		},
	}
	if err := s.upgradeLocked(); err == nil || !strings.Contains(err.Error(), "still draining") {
		t.Fatalf("upgrade did not respect the live generation cap: %v", err)
	}
	if err := s.startCanary(0, nil, 0); err == nil || !strings.Contains(err.Error(), "still draining") {
		t.Fatalf("canary did not respect the live generation cap: %v", err)
	}
	if s.router.Active().ID != "active" || s.liveWorkerCount() != 2 {
		t.Fatal("refused upgrade changed a serving generation")
	}
	close(s.workers["active"].done)
	if err := s.workerCapacityAvailable(); err != nil {
		t.Fatalf("dead active worker prevented recovery: %v", err)
	}
}
