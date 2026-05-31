package main

// nixneo4j.go — Docker-free neo4j runtime (mirrors postgres' nixpg.go).
//
// The neo4j service agent runs the database in a container by default
// (NewDockerHeadlessEnvironment). On hosts without Docker, the same agent can
// run neo4j NATIVELY from a nix-provisioned binary: the codefly NixEnvironment
// materializes `neo4j` (Community, 2026.02 / 5.x config line) from the embedded
// flake (no system install required), and this file drives the native lifecycle
// the Docker image's entrypoint would otherwise handle — seed a WRITABLE conf +
// data/logs/run dir (the nix-store home is read-only), launch `neo4j console`,
// and wait for the bolt connector to accept connections.
//
// nixpkgs neo4j is Community edition. Mind uses the default `neo4j` database
// (databases: []), so the Enterprise-only multi-database feature (CREATE
// DATABASE) is not needed; database creation, when requested, still runs through
// the agent's WaitForReady (and is a no-op on Community for the empty list).
//
// Both runtimes serve bolt + http on the same ports, so the rest of the agent
// (readiness, configuration) is unchanged.

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

//go:embed nix/flake.nix
var neo4jFlakeNix string

//go:embed nix/flake.lock
var neo4jFlakeLock string

// nixNeo4j runs a native neo4j server off a nix-provisioned binary.
type nixNeo4j struct {
	env      *runners.NixEnvironment
	flakeDir string
	// homeDir is the writable base holding conf/, logs/, run/, import/, plugins/.
	homeDir  string
	confDir  string
	dataDir  string
	boltPort uint16
	httpPort uint16
	out      io.Writer
	proc     runners.Proc

	// binDir is the absolute nix store bin dir holding the `neo4j` wrapper +
	// neo4j-admin. The wrapper bakes in JAVA_HOME, so invoking it by absolute
	// path runs the bundled JVM regardless of host PATH.
	binDir string
	// confSrc is the absolute nix store default conf dir (share/neo4j/conf),
	// copied into the writable confDir so the launcher's full config set
	// (neo4j.conf + the log4j XMLs) lives somewhere writable.
	confSrc string
}

// newNixNeo4j materializes the embedded flake under baseDir/nix and prepares a
// native neo4j rooted at baseDir/neo4j. baseDir is the agent's local service
// dir, so data persists across restarts exactly like the Docker volume mount.
func newNixNeo4j(ctx context.Context, baseDir string, boltPort, httpPort uint16, dataDir string, out io.Writer) (*nixNeo4j, error) {
	flakeDir := filepath.Join(baseDir, "nix")
	if err := os.MkdirAll(flakeDir, 0o755); err != nil {
		return nil, fmt.Errorf("create nix flake dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(flakeDir, "flake.nix"), []byte(neo4jFlakeNix), 0o644); err != nil {
		return nil, fmt.Errorf("write flake.nix: %w", err)
	}
	if err := os.WriteFile(filepath.Join(flakeDir, "flake.lock"), []byte(neo4jFlakeLock), 0o644); err != nil {
		return nil, fmt.Errorf("write flake.lock: %w", err)
	}
	env, err := runners.NewNixEnvironment(ctx, flakeDir)
	if err != nil {
		return nil, fmt.Errorf("nix environment (is nix installed?): %w", err)
	}
	env.WithCacheDir(filepath.Join(baseDir, ".nix-cache"))

	home := filepath.Join(baseDir, "neo4j")
	if dataDir == "" {
		dataDir = filepath.Join(home, "data")
	}
	return &nixNeo4j{
		env:      env,
		flakeDir: flakeDir,
		homeDir:  home,
		confDir:  filepath.Join(home, "conf"),
		dataDir:  dataDir,
		boltPort: boltPort,
		httpPort: httpPort,
		out:      out,
	}, nil
}

// Init materializes the nix env, seeds a writable conf + data layout, launches
// `neo4j console` in the background, and waits for the bolt connector.
func (n *nixNeo4j) Init(ctx context.Context) error {
	if err := n.env.Init(ctx); err != nil {
		return fmt.Errorf("materialize nix neo4j env: %w", err)
	}
	if err := n.resolveStore(); err != nil {
		return err
	}
	if err := n.seedConfig(); err != nil {
		return err
	}
	if err := n.startServer(ctx); err != nil {
		return err
	}
	return n.waitReady(ctx)
}

// resolveStore locates the nix store neo4j install: the `neo4j` wrapper bin (+
// neo4j-admin sibling) and the default conf dir to seed from. Using absolute
// store paths — rather than bare commands on PATH — guarantees we run the
// nix-built neo4j with its bundled JVM even if a system neo4j shadows PATH.
func (n *nixNeo4j) resolveStore() error {
	matches, err := filepath.Glob("/nix/store/*-neo4j-*/bin/neo4j")
	if err != nil {
		return fmt.Errorf("glob nix neo4j: %w", err)
	}
	for _, m := range matches {
		bin := filepath.Dir(m)
		if _, err := os.Stat(filepath.Join(bin, "neo4j-admin")); err != nil {
			continue // skip lib-only / partial outputs
		}
		confSrc := filepath.Join(filepath.Dir(bin), "share", "neo4j", "conf")
		if _, err := os.Stat(filepath.Join(confSrc, "neo4j.conf")); err != nil {
			continue
		}
		n.binDir = bin
		n.confSrc = confSrc
		return nil
	}
	return fmt.Errorf("no nix neo4j with bin/neo4j + share/neo4j/conf found in /nix/store (materialization may have failed)")
}

// seedConfig creates the writable dir layout, copies the store's default conf
// files into a writable conf dir, and appends the codefly overrides (ports,
// auth off, writable directories). The nix-store NEO4J_HOME is read-only, so
// every directory neo4j writes to MUST be redirected here.
func (n *nixNeo4j) seedConfig() error {
	logsDir := filepath.Join(n.homeDir, "logs")
	runDir := filepath.Join(n.homeDir, "run")
	importDir := filepath.Join(n.homeDir, "import")
	pluginsDir := filepath.Join(n.homeDir, "plugins")
	for _, d := range []string{n.confDir, n.dataDir, logsDir, runDir, importDir, pluginsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}

	// The codefly overrides: auth disabled (local dev runtime, matches the Docker
	// path's NEO4J_AUTH=none); bolt/http bound to the agent-assigned ports on
	// loopback; every writable directory redirected out of the read-only nix
	// store. server.directories.lib is intentionally NOT overridden — it resolves
	// against NEO4J_HOME (the store), where the jars live.
	overrides := map[string]string{
		"server.default_listen_address":            "127.0.0.1",
		"server.bolt.listen_address":               fmt.Sprintf("127.0.0.1:%d", n.boltPort),
		"server.http.listen_address":               fmt.Sprintf("127.0.0.1:%d", n.httpPort),
		"server.https.enabled":                     "false",
		"dbms.security.auth_enabled":               "false",
		"server.directories.data":                  n.dataDir,
		"server.directories.logs":                  logsDir,
		"server.directories.run":                   runDir,
		"server.directories.import":                importDir,
		"server.directories.plugins":               pluginsDir,
		"server.directories.transaction.logs.root": filepath.Join(n.dataDir, "transactions"),
	}

	// Copy the default conf set (neo4j.conf + the log4j XMLs) into the writable
	// conf dir. For neo4j.conf, MERGE the overrides rather than appending:
	// Neo4j rejects a key declared twice as a fatal config error, and the stock
	// conf ships some keys (e.g. server.directories.import) uncommented.
	entries, err := os.ReadDir(n.confSrc)
	if err != nil {
		return fmt.Errorf("read default conf: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(n.confSrc, e.Name()))
		if err != nil {
			return fmt.Errorf("read default conf %s: %w", e.Name(), err)
		}
		if e.Name() == "neo4j.conf" {
			data = []byte(mergeNeo4jConf(string(data), overrides))
		}
		if err := os.WriteFile(filepath.Join(n.confDir, e.Name()), data, 0o644); err != nil {
			return fmt.Errorf("write conf %s: %w", e.Name(), err)
		}
	}
	return nil
}

// mergeNeo4jConf drops any non-comment line that sets a key present in
// overrides, then appends the overrides as a trailing block in deterministic
// order. Neo4j treats a key declared twice as a fatal config error, so the
// overrides must REPLACE (not duplicate) the stock values.
func mergeNeo4jConf(conf string, overrides map[string]string) string {
	var b strings.Builder
	for _, line := range strings.Split(conf, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			if key, _, ok := strings.Cut(trimmed, "="); ok {
				if _, overridden := overrides[strings.TrimSpace(key)]; overridden {
					continue // dropped — re-set in the override block below
				}
			}
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n# --- codefly nix runtime overrides ---\n")
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(k + "=" + overrides[k] + "\n")
	}
	return b.String()
}

// startServer launches `neo4j console` (foreground server) in the background,
// pointing NEO4J_CONF at the writable conf dir. The wrapper bin sets JAVA_HOME.
func (n *nixNeo4j) startServer(ctx context.Context) error {
	proc, err := n.env.NewProcess(filepath.Join(n.binDir, "neo4j"), "console")
	if err != nil {
		return err
	}
	// NEO4J_CONF redirects the launcher to our writable conf dir (with the
	// overridden ports + directories). Inherited through the wrapper's exec.
	proc.WithEnvironmentVariables(ctx, resources.Env("NEO4J_CONF", n.confDir))
	if n.out != nil {
		proc.WithOutput(n.out)
	}
	if err := proc.Start(ctx); err != nil {
		return fmt.Errorf("start neo4j: %w", err)
	}
	n.proc = proc
	return nil
}

func (n *nixNeo4j) boltURI() string {
	return fmt.Sprintf("bolt://127.0.0.1:%d", n.boltPort)
}

// waitReady polls the bolt connector until neo4j accepts connections. Neo4j is
// slower to boot than postgres (JVM + store recovery), so the deadline is 60s.
func (n *nixNeo4j) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		driver, err := neo4j.NewDriverWithContext(n.boltURI(), neo4j.NoAuth())
		if err == nil {
			vctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			lastErr = driver.VerifyConnectivity(vctx)
			cancel()
			_ = driver.Close(ctx)
			if lastErr == nil {
				return nil
			}
		} else {
			lastErr = err
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("neo4j did not become ready on %s: %w", n.boltURI(), lastErr)
}

// Stop terminates the neo4j server process group.
func (n *nixNeo4j) Stop(ctx context.Context) error {
	if n.proc == nil {
		return nil
	}
	return n.proc.Stop(ctx)
}
