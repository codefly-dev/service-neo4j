package main

import (
	"context"
	"embed"
	"fmt"
	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/standards"
	"github.com/codefly-dev/core/templates"
)

type Builder struct {
	services.BuilderServer
	*Service
	NetworkMappings []*basev0.NetworkMapping
}

func NewBuilder(svc *Service) *Builder {
	return &Builder{
		Service: svc,
	}
}

func (s *Builder) Load(ctx context.Context, req *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	defer s.Wool.Catch()

	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return nil, err
	}

	requirements.Localize(s.Location)

	if req.CreationMode != nil {
		s.Builder.CreationMode = req.CreationMode
		s.Builder.GettingStarted, err = templates.ApplyTemplateFrom(ctx, shared.Embed(factoryFS), "templates/factory/GETTING_STARTED.md", s.Information)
		if err != nil {
			return nil, err
		}
		return s.Builder.LoadResponse()
	}

	s.Endpoints, err = s.Base.Service.LoadEndpoints(ctx)
	if err != nil {
		return s.Builder.LoadError(err)
	}

	return s.Builder.LoadResponse()
}

func (s *Builder) Init(ctx context.Context, req *builderv0.InitRequest) (*builderv0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	return s.Builder.InitResponse()
}

func (s *Builder) Update(ctx context.Context, req *builderv0.UpdateRequest) (*builderv0.UpdateResponse, error) {
	defer s.Wool.Catch()
	return &builderv0.UpdateResponse{}, nil
}

func (s *Builder) Sync(ctx context.Context, req *builderv0.SyncRequest) (*builderv0.SyncResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	return s.Builder.SyncResponse()
}

func (s *Builder) Build(ctx context.Context, req *builderv0.BuildRequest) (*builderv0.BuildResponse, error) {
	return s.Builder.BuildResponse()
}

func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()

	var k *builderv0.KubernetesDeployment
	var err error
	if k, err = s.Builder.KubernetesDeploymentRequest(ctx, req); err != nil {
		return s.Builder.DeployError(err)
	}

	namespace := k.Namespace
	boltService := fmt.Sprintf("bolt://neo4j-%s.%s.svc.cluster.local:7687", s.Base.Service.Name, namespace)
	httpService := fmt.Sprintf("http://neo4j-%s.%s.svc.cluster.local:7474", s.Base.Service.Name, namespace)

	conf := &basev0.Configuration{
		Origin: s.Base.Unique(),
		Infos: []*basev0.ConfigurationInformation{
			{
				Name: "bolt",
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: "connection", Value: boltService, Secret: true},
				},
			},
			{
				Name: "http",
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: "connection", Value: httpService, Secret: true},
				},
			},
		},
	}

	err = s.EnvironmentVariables.AddConfigurations(ctx, conf)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	s.Configuration = conf

	configs, err := s.EnvironmentVariables.Configurations()
	if err != nil {
		return s.Builder.DeployError(err)
	}
	cm, err := services.EnvsAsConfigMapData(configs...)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	secrets, err := services.EnvsAsSecretData(s.EnvironmentVariables.Secrets()...)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	params := services.DeploymentParameters{
		ConfigMap: cm,
		SecretMap: secrets,
	}

	err = s.Builder.KustomizeDeploy(ctx, req.Environment, k, deploymentFS, params)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	return s.Builder.DeployResponse()
}

func (s *Builder) Create(ctx context.Context, req *builderv0.CreateRequest) (*builderv0.CreateResponse, error) {
	defer s.Wool.Catch()

	err := s.Templates(ctx, nil, services.WithBuilder(builderFS))
	if err != nil {
		return s.Builder.CreateError(err)
	}

	err = s.CreateEndpoints(ctx)
	if err != nil {
		return s.Builder.CreateError(err)
	}

	return s.Builder.CreateResponse(ctx, s.Settings)
}

func (s *Builder) CreateEndpoints(ctx context.Context) error {
	// Bolt endpoint (primary — used by neo4j-go-driver)
	boltBase := s.Base.BaseEndpoint(standards.TCP)
	boltBase.Name = "bolt"
	tcp, err := resources.LoadTCPAPI(ctx)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot load TCP api")
	}
	s.bolt, err = resources.NewAPI(ctx, boltBase, resources.ToTCPAPI(tcp))
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create bolt tcp endpoint")
	}

	// HTTP endpoint (browser UI, REST API)
	httpBase := s.Base.BaseEndpoint(standards.TCP)
	httpBase.Name = "http"
	s.http, err = resources.NewAPI(ctx, httpBase, resources.ToTCPAPI(tcp))
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create http tcp endpoint")
	}

	s.Endpoints = []*basev0.Endpoint{s.bolt, s.http}
	return nil
}

//go:embed templates/factory
var factoryFS embed.FS

//go:embed templates/builder
var builderFS embed.FS

//go:embed templates/deployment
var deploymentFS embed.FS
