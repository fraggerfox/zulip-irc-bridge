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
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        go = pkgs.go_1_27;
      in
      {
        packages.default = pkgs.buildGoModule.override { go = go; } {
          pname = "zulip-irc-bridge";
          version = "0.1.0";
          src = ./.;

          # Update with the value nix prints on first build whenever
          # go.mod dependencies change.
          vendorHash = pkgs.lib.fakeHash;

          meta = {
            description = "Two-way bridge between Zulip streams and IRC channels";
            mainProgram = "zulip-irc-bridge";
          };
        };

        devShells.default = pkgs.mkShell {
          packages = [
            go
            pkgs.gopls
            pkgs.gotools
            pkgs.go-tools # staticcheck
          ];
        };
      }
    );
}
