package main

import (
	"context"
	"fmt"
	"os"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/shared"
	"github.com/hashicorp/go-multierror"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/codefly-dev/core/agents/services"
)

type Runtime struct {
	services.RuntimeServer
	*Service

	// internal
	runner   runners.RunnerEnvironment
	boltPort uint16
	httpPort uint16
}

func NewRuntime(svc *Service) *Runtime {
	return &Runtime{
		Service: svc,
	}
}

func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.SetEnvironment(req.Environment)

	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return s.Runtime.LoadErrorf(err, "loading base")
	}

	requirements.Localize(s.Location)

	s.Endpoints, err = s.Base.Service.LoadEndpoints(ctx)
	if err != nil {
		return s.Runtime.LoadErrorf(err, "loading endpoints")
	}

	s.bolt, err = resources.FindTCPEndpointWithName(ctx, "bolt", s.Endpoints)
	if err != nil {
		return s.Runtime.LoadErrorf(err, "finding bolt endpoint")
	}

	s.http, err = resources.FindTCPEndpointWithName(ctx, "http", s.Endpoints)
	if err != nil {
		return s.Runtime.LoadErrorf(err, "finding http endpoint")
	}

	return s.Runtime.LoadResponse()
}

func (s *Runtime) CreateConnectionConfigurationInformation(_ context.Context, endpoint *basev0.Endpoint, instance *basev0.NetworkInstance) *basev0.ConfigurationInformation {
	var connection string
	switch endpoint.Name {
	case "bolt":
		connection = fmt.Sprintf("bolt://%s:%d", instance.Hostname, instance.Port)
	case "http":
		connection = fmt.Sprintf("http://%s:%d", instance.Hostname, instance.Port)
	}

	return &basev0.ConfigurationInformation{
		Name: endpoint.Name,
		ConfigurationValues: []*basev0.ConfigurationValue{
			{Key: "connection", Value: connection, Secret: true},
		},
	}
}

func (s *Runtime) CreateConnectionsConfiguration(runtimeContext *basev0.RuntimeContext, infos []*basev0.ConfigurationInformation) *basev0.Configuration {
	return &basev0.Configuration{
		Origin:         s.Base.Unique(),
		RuntimeContext: runtimeContext,
		Infos:          infos,
	}
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogInitRequest(req)

	// Bolt endpoint mapping
	_, err := resources.FindNetworkMapping(ctx, req.ProposedNetworkMappings, s.bolt)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	boltInstanceContainer, err := resources.FindNetworkInstanceInNetworkMappings(ctx, req.ProposedNetworkMappings, s.bolt, resources.NewContainerNetworkAccess())
	if err != nil {
		return s.Runtime.InitError(err)
	}

	// HTTP endpoint mapping
	_, err = resources.FindNetworkMapping(ctx, req.ProposedNetworkMappings, s.http)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	httpInstanceContainer, err := resources.FindNetworkInstanceInNetworkMappings(ctx, req.ProposedNetworkMappings, s.http, resources.NewContainerNetworkAccess())
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.boltPort = 7687
	s.httpPort = 7474

	s.Infof("bolt endpoint will run on localhost:%d", boltInstanceContainer.Port)
	s.Infof("http endpoint will run on localhost:%d", httpInstanceContainer.Port)

	// Single container with both ports
	runner, err := runners.NewDockerHeadlessEnvironment(ctx, image, s.UniqueWithWorkspace())
	if err != nil {
		return s.Runtime.InitError(err)
	}

	runner.WithPortMapping(ctx, uint16(boltInstanceContainer.Port), s.boltPort)
	runner.WithPortMapping(ctx, uint16(httpInstanceContainer.Port), s.httpPort)
	runner.WithEnvironmentVariables(ctx, resources.Env("NEO4J_AUTH", "none")) // no auth for local dev
	runner.WithOutput(s.Logger)

	// Persist data across restarts
	dataDir := s.Settings.DataDir
	if dataDir == "" {
		// Default: ~/.codefly/data/{workspace}/{service}/neo4j
		home, _ := os.UserHomeDir()
		dataDir = fmt.Sprintf("%s/.codefly/data/%s/%s/neo4j", home, s.Identity.Workspace, s.Identity.Name)
	} else {
		dataDir = shared.Must(shared.SolvePath(dataDir))
	}
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return s.Runtime.InitError(fmt.Errorf("create data dir %s: %w", dataDir, err))
	}
	runner.WithMount(dataDir, "/data")
	s.Infof("persisting Neo4j data to %s", dataDir)

	err = runner.Init(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.runner = runner

	// Build network mappings
	boltMapping, err := resources.FindNetworkMapping(ctx, req.ProposedNetworkMappings, s.bolt)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	httpMapping, err := resources.FindNetworkMapping(ctx, req.ProposedNetworkMappings, s.http)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.NetworkMappings = []*basev0.NetworkMapping{boltMapping, httpMapping}

	// Build configurations for each runtime context
	informations := make(map[string][]*basev0.ConfigurationInformation)

	for _, inst := range boltMapping.Instances {
		informations[resources.RuntimeContextFromInstance(inst).Kind] = append(informations[resources.RuntimeContextFromInstance(inst).Kind], s.CreateConnectionConfigurationInformation(ctx, s.bolt, inst))
	}
	for _, inst := range httpMapping.Instances {
		informations[resources.RuntimeContextFromInstance(inst).Kind] = append(informations[resources.RuntimeContextFromInstance(inst).Kind], s.CreateConnectionConfigurationInformation(ctx, s.http, inst))
	}
	for _, runtimeContext := range []*basev0.RuntimeContext{resources.NewRuntimeContextNative(), resources.NewRuntimeContextContainer()} {
		conf := s.CreateConnectionsConfiguration(runtimeContext, informations[runtimeContext.Kind])
		s.Runtime.RuntimeConfigurations = append(s.Runtime.RuntimeConfigurations, conf)
	}

	return s.Runtime.InitResponse()
}

func (s *Runtime) WaitForReady(ctx context.Context) error {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	configuration, err := resources.ExtractConfiguration(s.Runtime.RuntimeConfigurations, resources.NewRuntimeContextNative())
	if err != nil {
		return err
	}

	connBoltString, err := resources.GetConfigurationValue(ctx, configuration, "bolt", "connection")
	if err != nil {
		return err
	}

	// Try connecting with the neo4j driver
	return shared.Retry(5*time.Second, 10, func() error {
		driver, err := neo4j.NewDriverWithContext(connBoltString, neo4j.NoAuth())
		if err != nil {
			return err
		}
		defer driver.Close(ctx)
		return driver.VerifyConnectivity(ctx)
	})
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	err := s.WaitForReady(ctx)
	if err != nil {
		return s.Runtime.StartError(err)
	}

	return s.Runtime.StartResponse()
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return s.Runtime.InformationResponse(ctx, req)
}

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()

	s.Wool.Debug("nothing to stop: keep environment alive")

	return s.Runtime.StopResponse()
}

func (s *Runtime) Destroy(ctx context.Context, req *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	if s.runner != nil {
		err := s.runner.Stop(ctx)
		if err != nil {
			var agg error
			agg = multierror.Append(agg, err)
			return s.Runtime.DestroyError(agg)
		}
	}
	return s.Runtime.DestroyResponse()
}

func (s *Runtime) Test(ctx context.Context, req *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	return s.Runtime.TestResponse()
}
