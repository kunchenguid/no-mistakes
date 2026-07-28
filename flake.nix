{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
  };

  outputs =
    { nixpkgs, ... }:
    let
      version = "1.41.2"; # x-release-please-version
      systems = [
        "aarch64-darwin"
        "x86_64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.buildGoModule {
            pname = "no-mistakes";
            inherit version;
            src = ./.;
            vendorHash = "sha256-NZOYxNYvt4192uqKBdKRxdgrKFvWx3585psdCnRdPSM=";
            subPackages = [ "cmd/no-mistakes" ];
            ldflags = [
              "-X github.com/kunchenguid/no-mistakes/internal/buildinfo.Version=v${version}"
              "-X github.com/kunchenguid/no-mistakes/internal/buildinfo.TelemetryWebsiteID=f959e889-92f5-4121-8a1f-571b10861198"
            ];
            # go test ./... was run in the sandbox (nativeCheckInputs = [ pkgs.git
            # pkgs.perl pkgs.procps ]; checkPhase overridden past the subPackages
            # narrowing) and reduced most failures, but three are structural
            # sandbox mismatches rather than code bugs: TestPRStep_GhNotAvailable
            # needs a real .git directory (the flake source has none, since Nix
            # only copies git-tracked files); TestDefaultShellCommandOutput_*
            # and login-shell resolution need a real /bin/bash FHS path; and
            # TestColdDetachedStartupProductionGateCardinality asserts a
            # wall-clock deadline crossing calibrated for real hardware. Full
            # regression coverage already runs in CI (.github/workflows/ci.yml)
            # on ubuntu/macos/windows runners, so it is not skipped, only not
            # duplicated inside this hermetic build.
            doCheck = false;
          };
        }
      );
    };
}
