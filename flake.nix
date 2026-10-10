{
  description = "no-mistakes: a local gate that reviews, tests, and fixes every push before it ships";

  inputs = {
    # No x86_64-darwin: nixpkgs-unstable dropped it in 26.11 and throws on
    # evaluation.
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-compat.url = "github:edolstra/flake-compat";
    flake-compat.flake = false;
  };

  outputs =
    { self, nixpkgs, ... }:
    let
      version = (builtins.fromJSON (builtins.readFile ./.release-please-manifest.json)).".";
      stamp = self.lastModifiedDate;
      inherit (nixpkgs.lib) substring;
      forAllSystems = nixpkgs.lib.genAttrs [
        "aarch64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
    in
    {
      packages = forAllSystems (
        system:
        let
          no-mistakes = nixpkgs.legacyPackages.${system}.callPackage ./package.nix {
            inherit version;
            commit = self.shortRev or self.dirtyShortRev or "unknown";
            date = "${substring 0 4 stamp}-${substring 4 2 stamp}-${substring 6 2 stamp}T${substring 8 2 stamp}:${substring 10 2 stamp}:${substring 12 2 stamp}Z";
          };
        in
        {
          inherit no-mistakes;
          default = no-mistakes;
        }
      );
    };
}
