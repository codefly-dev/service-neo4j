package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/stretchr/testify/require"
)

// TestCreateToRunDocker runs the full agent lifecycle against the Docker
// runtime (the default container backend).
func TestCreateToRunDocker(t *testing.T) {
	testCreateToRun(t, resources.NewRuntimeContextFree())
}

// TestCreateToRunNix runs the SAME full lifecycle against the nix runtime —
// the Docker-free backend in the docker/nix matrix. Requires nix.
func TestCreateToRunNix(t *testing.T) {
	if !runners.CheckNixInstalled() || !runners.IsNixSupported() {
		t.Skip("nix not installed/supported on this host")
	}
	testCreateToRun(t, resources.NewRuntimeContextNix())
}

// testCreateToRun drives Load → Create → Init → Start → connect (bolt) →
// RETURN 1 for one runtime context, so docker and nix exercise the identical
// agent path.
func testCreateToRun(t *testing.T, runtimeContext *basev0.RuntimeContext) {
	wool.SetGlobalLogLevel(wool.DEBUG)
	ctx := context.Background()

	workspace := &resources.Workspace{Name: "test"}

	tmpDir := t.TempDir()
	defer func(p string) {
		require.NoError(t, os.RemoveAll(p))
	}(tmpDir)

	serviceName := fmt.Sprintf("svc-%v", time.Now().UnixMilli())
	service := resources.Service{Name: serviceName, Version: "test-me"}
	err := service.SaveAtDir(ctx, path.Join(tmpDir, "mod", service.Name))
	require.NoError(t, err)

	identity := &basev0.ServiceIdentity{
		Name:                service.Name,
		Module:              "mod",
		Workspace:           workspace.Name,
		WorkspacePath:       tmpDir,
		RelativeToWorkspace: fmt.Sprintf("mod/%s", service.Name),
	}

	builder := NewBuilder(NewService())
	_, err = builder.Load(ctx, &builderv0.LoadRequest{DisableCatch: true, Identity: identity, CreationMode: &builderv0.CreationMode{Communicate: false}})
	require.NoError(t, err)

	_, err = builder.Create(ctx, &builderv0.CreateRequest{})
	require.NoError(t, err)

	// Now run it
	runtime := NewRuntime(NewService())

	networkManager, err := network.NewRuntimeManager(ctx, nil)
	require.NoError(t, err)
	networkManager.WithTemporaryPorts()

	env := resources.LocalEnvironment()

	_, err = runtime.Load(ctx, &runtimev0.LoadRequest{
		Identity:     identity,
		Environment:  shared.Must(env.Proto()),
		DisableCatch: true})
	require.NoError(t, err)

	// neo4j exposes two endpoints: bolt + http.
	require.Equal(t, 2, len(runtime.Endpoints))

	networkMappings, err := networkManager.GenerateNetworkMappings(ctx, env, workspace, runtime.Identity, runtime.Endpoints, runtimeContext)
	require.NoError(t, err)
	require.Equal(t, 2, len(networkMappings))

	init, err := runtime.Init(ctx, &runtimev0.InitRequest{
		RuntimeContext:          runtimeContext,
		ProposedNetworkMappings: networkMappings,
	})
	require.NoError(t, err)
	require.NotNil(t, init)

	defer func() {
		_, _ = runtime.Destroy(ctx, &runtimev0.DestroyRequest{})
	}()

	// Init reports failures through the response status (a nil Go error), so
	// assert readiness here — otherwise a failed Init surfaces only far
	// downstream as a misleading "configuration is nil". Registered after the
	// Destroy defer so a failed assertion still tears down the data dir.
	require.Equal(t, runtimev0.InitStatus_READY, init.GetStatus().GetState(), init.GetStatus().GetMessage())

	start, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, start.GetStatus().GetState(), start.GetStatus().GetMessage())

	// Get the native bolt connection string and run a Cypher query.
	configurationOut, err := resources.ExtractConfiguration(init.RuntimeConfigurations, resources.NewRuntimeContextNative())
	require.NoError(t, err)

	boltConn, err := resources.GetConfigurationValue(ctx, configurationOut, "bolt", "connection")
	require.NoError(t, err)

	driver, err := neo4j.NewDriverWithContext(boltConn, neo4j.NoAuth())
	require.NoError(t, err)
	defer driver.Close(ctx)

	require.NoError(t, driver.VerifyConnectivity(ctx))

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx)

	result, err := session.Run(ctx, "RETURN 1 AS n", nil)
	require.NoError(t, err)

	record, err := result.Single(ctx)
	require.NoError(t, err)
	val, ok := record.Get("n")
	require.True(t, ok)
	require.Equal(t, int64(1), val)
}

// exitedProc is a runners.Proc that reports the process is not running, used to
// drive nixNeo4j.waitReady's process-liveness fast-fail without a real server.
type exitedProc struct{}

func (exitedProc) Start(context.Context) error             { return nil }
func (exitedProc) Run(context.Context) error               { return nil }
func (exitedProc) Stop(context.Context) error              { return nil }
func (exitedProc) Wait(context.Context) error              { return nil }
func (exitedProc) IsRunning(context.Context) (bool, error) { return false, nil }
func (exitedProc) WaitOn(string)                           {}
func (exitedProc) WithDir(string)                          {}
func (exitedProc) WithOutput(io.Writer)                    {}
func (exitedProc) WithEnvironmentVariables(context.Context, ...*resources.EnvironmentVariable) {
}
func (exitedProc) WithEnvironmentVariablesAppend(context.Context, *resources.EnvironmentVariable, string) {
}
func (exitedProc) StdinPipe() (io.WriteCloser, error) { return nil, nil }
func (exitedProc) StdoutPipe() (io.ReadCloser, error) { return nil, nil }

// TestNixWaitReadyFailsFastWhenProcessExits asserts waitReady returns promptly
// once the server process is gone, instead of polling until its (5-minute)
// deadline. Without the liveness check a crashed neo4j stalls the whole Init.
func TestNixWaitReadyFailsFastWhenProcessExits(t *testing.T) {
	n := &nixNeo4j{proc: exitedProc{}, boltPort: 1, out: io.Discard}

	done := make(chan error, 1)
	go func() { done <- n.waitReady(context.Background()) }()

	select {
	case err := <-done:
		require.Error(t, err)
		require.Contains(t, err.Error(), "process exited")
	case <-time.After(30 * time.Second):
		t.Fatal("waitReady did not fail fast after the process exited")
	}
}

// TestNixInitOwnsProcessOnFailedReadiness drives the nix runtime with its bolt
// port already taken, so neo4j starts but can never bind bolt and exits. Init
// must (a) fail fast rather than block for the full readiness deadline, and
// (b) have registered the runtime for teardown before waiting — otherwise the
// started neo4j process is orphaned. It also confirms the failure surfaces as a
// non-READY Init status, not the misleading downstream "configuration is nil".
func TestNixInitOwnsProcessOnFailedReadiness(t *testing.T) {
	if !runners.CheckNixInstalled() || !runners.IsNixSupported() {
		t.Skip("nix not installed/supported on this host")
	}
	wool.SetGlobalLogLevel(wool.DEBUG)
	ctx := context.Background()

	workspace := &resources.Workspace{Name: "test"}
	tmpDir := t.TempDir()

	serviceName := fmt.Sprintf("svc-%v", time.Now().UnixMilli())
	service := resources.Service{Name: serviceName, Version: "test-me"}
	require.NoError(t, service.SaveAtDir(ctx, path.Join(tmpDir, "mod", service.Name)))

	identity := &basev0.ServiceIdentity{
		Name:                service.Name,
		Module:              "mod",
		Workspace:           workspace.Name,
		WorkspacePath:       tmpDir,
		RelativeToWorkspace: fmt.Sprintf("mod/%s", service.Name),
	}

	builder := NewBuilder(NewService())
	_, err := builder.Load(ctx, &builderv0.LoadRequest{DisableCatch: true, Identity: identity, CreationMode: &builderv0.CreationMode{Communicate: false}})
	require.NoError(t, err)
	_, err = builder.Create(ctx, &builderv0.CreateRequest{})
	require.NoError(t, err)

	runtime := NewRuntime(NewService())
	networkManager, err := network.NewRuntimeManager(ctx, nil)
	require.NoError(t, err)
	networkManager.WithTemporaryPorts()

	env := resources.LocalEnvironment()
	_, err = runtime.Load(ctx, &runtimev0.LoadRequest{Identity: identity, Environment: shared.Must(env.Proto()), DisableCatch: true})
	require.NoError(t, err)

	runtimeContext := resources.NewRuntimeContextNix()
	networkMappings, err := networkManager.GenerateNetworkMappings(ctx, env, workspace, runtime.Identity, runtime.Endpoints, runtimeContext)
	require.NoError(t, err)

	// Occupy the bolt port so the launched neo4j can never bind it and exits.
	boltInst, err := resources.FindNetworkInstanceInNetworkMappings(ctx, networkMappings, runtime.bolt, resources.NewContainerNetworkAccess())
	require.NoError(t, err)
	blocker, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", boltInst.Port))
	require.NoError(t, err)
	defer func() { _ = blocker.Close() }()

	defer func() {
		_, _ = runtime.Destroy(ctx, &runtimev0.DestroyRequest{})
	}()

	start := time.Now()
	init, err := runtime.Init(ctx, &runtimev0.InitRequest{RuntimeContext: runtimeContext, ProposedNetworkMappings: networkMappings})
	require.NoError(t, err)
	require.NotNil(t, init)

	// Init must report the failure (not a healthy READY), and it must fail fast
	// once the process exits rather than poll the full 5-minute deadline. neo4j
	// reaches the bolt-bind step (and dies) only late in a slow boot (~2.5min
	// worst case), so the bound sits between that and the 5-minute deadline the
	// old, liveness-blind loop would have run to.
	require.Equal(t, runtimev0.InitStatus_ERROR, init.GetStatus().GetState())
	require.Less(t, time.Since(start), 4*time.Minute, "Init should fail fast once neo4j exits, not poll the full deadline")

	// The started process must be owned for teardown even though Init failed.
	require.NotNil(t, runtime.nixRuntime, "a failed Init must still register the nix runtime so Destroy can stop it")
}
