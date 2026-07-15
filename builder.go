package main

import (
	"context"
	"embed"
	"fmt"

	"github.com/codefly-dev/core/agents/communicate"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/agents/services/audit"
	"github.com/codefly-dev/core/agents/services/upgrade"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
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

	return s.Builder.LoadService(ctx, req, services.BuilderLoad{
		Settings:         s.Settings,
		Requirements:     requirements,
		FactoryTemplates: factoryFS,
	})
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

// Audit scans the neo4j image for vulnerabilities via trivy.
func (s *Builder) Audit(ctx context.Context, req *builderv0.AuditRequest) (*builderv0.AuditResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	res, err := audit.Docker(ctx, image.FullName())
	if err != nil {
		return s.Builder.AuditError(err)
	}
	return s.Builder.AuditResponse(res.Findings, res.Outdated, res.Tool, res.Language)
}

// Upgrade reports a tag bump from the current neo4j image.
func (s *Builder) Upgrade(ctx context.Context, req *builderv0.UpgradeRequest) (*builderv0.UpgradeResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	res, err := upgrade.Docker(ctx, image.FullName(), upgrade.Options{
		IncludeMajor: req.IncludeMajor,
		DryRun:       req.DryRun,
	})
	if err != nil {
		return s.Builder.UpgradeError(err)
	}
	return s.Builder.UpgradeResponse(res.Changes, res.LockfileDiff)
}

func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()

	// Neo4j uses the stock Enterprise image in both local and Kubernetes modes;
	// configured databases rely on Enterprise-only CREATE DATABASE support.
	s.Base.SetDockerImage(image)

	return s.Builder.DeployKustomize(ctx, req, services.KustomizeDeployment{
		EnvironmentVariables: s.EnvironmentVariables,
		Templates:            deploymentFS,
		Prepare: func(ctx context.Context, deployment *services.KustomizeDeploymentContext) error {
			namespace := deployment.Kubernetes.GetNamespace()
			boltService := fmt.Sprintf("bolt://neo4j-%s.%s.svc.cluster.local:7687", s.Base.Service.Name, namespace)
			httpService := fmt.Sprintf("http://neo4j-%s.%s.svc.cluster.local:7474", s.Base.Service.Name, namespace)
			configuration := &basev0.Configuration{
				Origin: s.Base.Unique(),
				Infos: []*basev0.ConfigurationInformation{
					{Name: "bolt", ConfigurationValues: []*basev0.ConfigurationValue{{Key: "connection", Value: boltService, Secret: true}}},
					{Name: "http", ConfigurationValues: []*basev0.ConfigurationValue{{Key: "connection", Value: httpService, Secret: true}}},
				},
			}
			return deployment.ExportConfiguration(ctx, configuration)
		},
	})
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

func (s *Builder) Communicate(stream builderv0.Builder_CommunicateServer) error {
	asker := communicate.NewQuestionAsker(stream)
	_, err := asker.RunSequence(nil)
	return err
}

//go:embed templates/factory
var factoryFS embed.FS

//go:embed templates/builder
var builderFS embed.FS

//go:embed templates/deployment
var deploymentFS embed.FS
