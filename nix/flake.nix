{
  description = "codefly neo4j service: nix runtime (Docker-free)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f system);
    in
    {
      # devShell exposes the neo4j server + admin CLIs (neo4j, neo4j-admin,
      # cypher-shell) so the codefly NixEnvironment runs them via the
      # materialized devShell. nixpkgs neo4j is Community edition; Mind uses
      # the default `neo4j` database (databases: []), so Community suffices.
      devShells = forAllSystems (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.mkShell {
            packages = [
              pkgs.neo4j
            ];
          };
        });
    };
}
