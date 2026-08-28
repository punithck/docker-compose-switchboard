package main

import (
	"context"
	"strings"
	"testing"
)

type fakeRunner struct {
	calls [][]string
	out   map[string]string
}

func (r *fakeRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, args)
	return []byte(r.out[strings.Join(args, " ")]), nil
}

func TestServicesUsesConfiguredMetadataAndComposeStatus(t *testing.T) {
	runner := &fakeRunner{
		out: map[string]string{
			"compose -f compose.yaml ps --all --format json": `[{"Service":"api","State":"running","Health":"healthy"}]`,
		},
	}
	server := &Server{
		config: Config{Projects: []ProjectConfig{
			{
				Name:         "demo",
				Workdir:      ".",
				ComposeFiles: []string{"compose.yaml"},
				Services: []ServiceConfig{
					{Name: "api", DisplayName: "API", Group: "core", Port: "8080"},
					{Name: "worker", Group: "jobs"},
				},
			},
		}},
		runner: runner,
	}

	services, err := server.services(context.Background())
	if err != nil {
		t.Fatalf("services returned error: %v", err)
	}

	if len(services) != 2 {
		t.Fatalf("expected 2 services, got %d", len(services))
	}
	if services[0].ID != "demo:api" || services[0].Status != "running" || services[0].Health != "healthy" {
		t.Fatalf("unexpected first service: %+v", services[0])
	}
	if services[1].ID != "demo:worker" || services[1].Status != "stopped" {
		t.Fatalf("unexpected second service: %+v", services[1])
	}
}

func TestRunActionStartsDependenciesAndService(t *testing.T) {
	runner := &fakeRunner{
		out: map[string]string{
			"compose -f compose.yaml up -d postgres api": "started",
		},
	}
	server := &Server{
		config: Config{Projects: []ProjectConfig{
			{
				Name:         "demo",
				Workdir:      ".",
				ComposeFiles: []string{"compose.yaml"},
				Services: []ServiceConfig{
					{Name: "api", DependsOn: []string{"postgres"}},
				},
			},
		}},
		runner: runner,
	}

	output, err := server.runAction(context.Background(), "demo:api", "start")
	if err != nil {
		t.Fatalf("runAction returned error: %v", err)
	}
	if output != "started" {
		t.Fatalf("unexpected output: %q", output)
	}

	got := strings.Join(runner.calls[0], " ")
	want := "compose -f compose.yaml up -d postgres api"
	if got != want {
		t.Fatalf("unexpected docker call: got %q want %q", got, want)
	}
}
