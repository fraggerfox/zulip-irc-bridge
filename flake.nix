{
  description = "Two-way bridge between Zulip streams and IRC channels";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
    }:
    {
      nixosModules.default = import ./nix/module.nix { selfPackages = self.packages; };
    }
    // flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        go = pkgs.go; # 1.26.x — matches go.mod directive and golangci-lint build
      in
      {
        packages.default = pkgs.buildGoModule.override { go = go; } {
          pname = "zulip-irc-bridge";
          version = "0.1.0"; # x-release-please-version
          src = ./.;

          vendorHash = "sha256-oGc1Zx+FSQOj+bFSX0nEjpuNgzlhe9bu/bSRP5FNp7U=";

          meta = {
            description = "Two-way bridge between Zulip streams and IRC channels";
            license = pkgs.lib.licenses.bsd2;
            mainProgram = "zulip-irc-bridge";
          };
        };

        devShells.default = pkgs.mkShell {
          packages = [
            go
            pkgs.gopls
            pkgs.gotools
            pkgs.go-tools # staticcheck
            pkgs.golangci-lint
          ];
        };
      }
    );
}
