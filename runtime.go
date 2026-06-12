package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	runner runners.RunnerEnvironment

	// nixRuntime is set instead of runner when the caller requests
	// RuntimeContextNix — neo4j then runs natively from a nix-provisioned
	// binary (no Docker), serving the same bolt/http connection strings.
	nixRuntime *nixNeo4j

	boltPort uint16
	httpPort uint16

	// dataDir is the resolved on-disk data directory (set in Init). Destroy
	// removes it so ephemeral / per-naming-scope runs don't leak data dirs
	// (each ~500MB–2GB) until the disk fills.
	dataDir string
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

	values := []*basev0.ConfigurationValue{
		{Key: "connection", Value: connection, Secret: true},
	}

	// Expose per-database connections: db/<name> = bolt://host:port/<dbname>
	if endpoint.Name == "bolt" {
		for _, db := range s.Settings.Databases {
			values = append(values, &basev0.ConfigurationValue{
				Key:    fmt.Sprintf("db/%s", db),
				Value:  fmt.Sprintf("bolt://%s:%d/%s", instance.Hostname, instance.Port, db),
				Secret: true,
			})
		}
	}

	return &basev0.ConfigurationInformation{
		Name:                endpoint.Name,
		ConfigurationValues: values,
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

	// Persist data across restarts — used by both the Docker and nix runtimes.
	dataDir := s.Settings.DataDir
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = defaultDataDir(home, s.Identity.Workspace, s.Identity.Name, s.Environment.NamingScope)
	} else {
		dataDir = shared.Must(shared.SolvePath(dataDir))
	}
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return s.Runtime.InitError(fmt.Errorf("create data dir %s: %w", dataDir, err))
	}
	s.dataDir = dataDir

	// Nix runtime: run neo4j natively from a nix-provisioned binary instead of a
	// Docker container — selected when the caller requests RuntimeContextNix
	// (e.g. a host without Docker). Same bolt/http connection strings, so the
	// configuration + readiness logic below is unchanged.
	if rc := req.GetRuntimeContext(); rc != nil && rc.Kind == resources.RuntimeContextNix {
		s.Infof("using nix runtime for neo4j (bolt %d, http %d)", boltInstanceContainer.Port, httpInstanceContainer.Port)
		nixn, err := newNixNeo4j(ctx, s.Location,
			uint16(boltInstanceContainer.Port), uint16(httpInstanceContainer.Port), dataDir, s.Logger)
		if err != nil {
			return s.Runtime.InitError(err)
		}
		if err := nixn.Init(ctx); err != nil {
			return s.Runtime.InitError(err)
		}
		s.nixRuntime = nixn
		s.Infof("persisting Neo4j data to %s", dataDir)
	} else {
		// Docker: single container with both ports.
		runner, err := runners.NewDockerHeadlessEnvironment(ctx, image, s.UniqueWithWorkspace())
		if err != nil {
			return s.Runtime.InitError(err)
		}

		runner.WithPortMapping(ctx, uint16(boltInstanceContainer.Port), s.boltPort)
		runner.WithPortMapping(ctx, uint16(httpInstanceContainer.Port), s.httpPort)
		runner.WithEnvironmentVariables(ctx, resources.Env("NEO4J_AUTH", "none"))
		runner.WithEnvironmentVariables(ctx, resources.Env("NEO4J_ACCEPT_LICENSE_AGREEMENT", "yes"))
		// Quieten the server when a log level is configured.
		//
		// IMPORTANT: in Neo4j 5.x the per-file log LEVELS (debug.log,
		// user.log) are controlled by log4j XML (`server-logs.xml`),
		// NOT by neo4j.conf — the old `dbms.logs.*.level` and the more
		// recent `server.logs.*.level` keys were both removed, and
		// `server.config.strict_validation` makes the container REFUSE
		// to boot if you set them. The only verbosity knob safely
		// expressible as an env-var override is the query log, which
		// is by far the noisiest channel in practice anyway.
		//
		// Going further (per-file levels) requires mounting a custom
		// log4j xml — out of scope for this yaml setting. The
		// LogLevel field is still honored so the agent contract stays
		// uniform with the postgres one; we just only apply the parts
		// that Neo4j 5.x exposes via env.
		if lvl := strings.ToUpper(strings.TrimSpace(s.Settings.LogLevel)); lvl != "" {
			runner.WithEnvironmentVariables(ctx,
				resources.Env("NEO4J_db_logs_query_enabled", "OFF"),
			)
		}
		runner.WithOutput(s.Logger)
		runner.WithMount(dataDir, "/data")
		s.Infof("persisting Neo4j data to %s", dataDir)

		if err = runner.Init(ctx); err != nil {
			return s.Runtime.InitError(err)
		}

		s.Infof("starting %s:%s", image.Name, image.Tag)
		s.runner = runner
	}

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

	// Wait for Neo4j to accept connections.
	var driver neo4j.DriverWithContext
	err = shared.Retry(5*time.Second, 12, func() error {
		var dErr error
		driver, dErr = neo4j.NewDriverWithContext(connBoltString, neo4j.NoAuth())
		if dErr != nil {
			return dErr
		}
		return driver.VerifyConnectivity(ctx)
	})
	if err != nil {
		// Surface container logs so the user sees the real cause
		// (license-not-accepted, port collision, plugin crash, etc.)
		// rather than just a bolt connection refused.
		if docker, ok := s.runner.(*runners.DockerEnvironment); ok {
			if tail := docker.TailLogs(ctx, 30); tail != "" {
				return fmt.Errorf("neo4j not ready: %w; container logs (tail 30):\n%s", err, tail)
			}
		}
		return err
	}
	defer driver.Close(ctx)

	// Create databases (Enterprise feature).
	for _, db := range s.Settings.Databases {
		session := driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: "system"})
		_, err := session.Run(ctx, fmt.Sprintf("CREATE DATABASE %s IF NOT EXISTS", db), nil)
		session.Close(ctx)
		if err != nil {
			return fmt.Errorf("create database %s: %w", db, err)
		}
		s.Infof("database %s ready", db)
	}

	return nil
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Infof("waiting for Neo4j to be ready...")
	err := s.WaitForReady(ctx)
	if err != nil {
		return s.Runtime.StartError(err)
	}

	s.Infof("Neo4j ready (bolt://localhost:%d, http://localhost:%d)", s.boltPort, s.httpPort)
	if len(s.Settings.Databases) > 0 {
		s.Infof("databases: %v", s.Settings.Databases)
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
	// Destroy means full teardown — remove the persisted data directory on
	// every path. Without this, each per-naming-scope run (e.g. mindtest)
	// leaked a ~500MB–2GB data dir that accumulated until the disk filled.
	defer s.removeDataDir()

	// Nix runtime: stop the native neo4j process group.
	if s.nixRuntime != nil {
		if err := s.nixRuntime.Stop(ctx); err != nil {
			return s.Runtime.DestroyError(err)
		}
		return s.Runtime.DestroyResponse()
	}
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

func (s *Runtime) removeDataDir() {
	if s.dataDir == "" {
		return
	}
	if err := os.RemoveAll(s.dataDir); err != nil {
		s.Wool.Warn(fmt.Sprintf("could not remove neo4j data dir %s: %v", s.dataDir, err))
	} else {
		s.Wool.Debug(fmt.Sprintf("removed neo4j data dir %s", s.dataDir))
	}
}

func (s *Runtime) Test(ctx context.Context, req *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	return s.Runtime.TestResponse()
}

func defaultDataDir(home, workspace, service, namingScope string) string {
	scopedService := service
	if namingScope != "" {
		scopedService = fmt.Sprintf("%s-%s", service, namingScope)
	}
	return filepath.Join(home, ".codefly", "data", workspace, scopedService, "neo4j")
}
