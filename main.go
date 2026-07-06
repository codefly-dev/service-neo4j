package main

import (
	"context"
	"embed"

	"github.com/codefly-dev/core/builders"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/templates"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/services"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
)

// Agent version
var agent = shared.Must(resources.LoadFromFs[resources.Agent](shared.Embed(infoFS)))

var requirements = builders.NewDependencies(agent.Name,
	builders.NewDependency("service.codefly.yaml"),
)

type Settings struct {
	Watch     bool     `yaml:"watch"`
	DataDir   string   `yaml:"data-dir"`
	Databases []string `yaml:"databases"`

	// LogLevel controls Neo4j server log verbosity. When set, the
	// agent passes NEO4J_server_logs_user_level and disables query
	// logging so day-to-day startup/heartbeat noise stays out of the
	// codefly forwarder. Accepts Neo4j's levels: DEBUG, INFO, WARN,
	// ERROR. Empty = Neo4j default (INFO, which is chatty).
	LogLevel string `yaml:"log-level"`
}

// 5.26 is the current Neo4j 5.x LTS. Enterprise is required for the
// multi-database (CREATE DATABASE) feature the runtime uses.
var image = &resources.DockerImage{Name: "neo4j", Tag: "5.26-enterprise"}

type Service struct {
	*services.Base

	// Settings
	*Settings

	bolt *basev0.Endpoint
	http *basev0.Endpoint
}

func (s *Service) GetAgentInformation(ctx context.Context, _ *agentv0.AgentInformationRequest) (*agentv0.AgentInformation, error) {

	readme, err := templates.ApplyTemplateFrom(ctx, shared.Embed(readmeFS), "templates/agent/README.md", s.Information)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &agentv0.AgentInformation{
		// Advertise the nix runtime (implemented in nixneo4j.go via
		// RuntimeContextNix) so the CLI's per-service Docker-free gate
		// (flow.resolveDockerFallback → Runner.SupportsNix) lets this service
		// fall back to a nix-provisioned native process when Docker is
		// unreachable. Without it the run hard-stops with "requires Docker"
		// even though the nix path works.
		RuntimeRequirements: []*agentv0.Runtime{
			{Type: agentv0.Runtime_NIX},
		},
		Capabilities: []*agentv0.Capability{
			{Type: agentv0.Capability_BUILDER},
			{Type: agentv0.Capability_RUNTIME},
		},
		Protocols: []*agentv0.Protocol{},
		ConfigurationDetails: []*agentv0.ConfigurationValueDetail{
			{
				Name: "connection", Description: "connection details",
				Fields: []*agentv0.ConfigurationValueInformation{
					{Name: "bolt", Description: "bolt protocol connection URI"},
					{Name: "http", Description: "http connection URI"},
				},
			},
		},
		ReadMe: readme,
	}, nil
}

func NewService() *Service {
	return &Service{
		Base:     services.NewServiceBase(context.Background(), agent.Of(resources.ServiceAgent)),
		Settings: &Settings{},
	}
}

func main() {
	svc := NewService()
	agents.Serve(agents.PluginRegistration{
		Agent:   svc,
		Runtime: NewRuntime(svc),
		Builder: NewBuilder(svc),
	})
}

//go:embed agent.codefly.yaml
var infoFS embed.FS

//go:embed templates/agent
var readmeFS embed.FS
