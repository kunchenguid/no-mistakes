{
  description = "no-mistakes development environment";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
      in
      {
        # go.mod requires `go 1.25.0`; go_1_25 keeps this dev shell in sync
        # with that toolchain instead of relying on the machine's own PATH.
        devShells.default = pkgs.mkShell {
          buildInputs = [ pkgs.go_1_25 ];
        };
      }
    );
}
