{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
  };

  outputs =
    { self, nixpkgs, ... }:
    let
      version = "1.41.2"; # x-release-please-version
      commit = self.shortRev or self.dirtyShortRev or "unknown";
      stamp = self.lastModifiedDate;
      inherit (nixpkgs.lib) substring;
      date = "${substring 0 4 stamp}-${substring 4 2 stamp}-${substring 6 2 stamp}T${substring 8 2 stamp}:${substring 10 2 stamp}:${substring 12 2 stamp}Z";
      # Nixpkgs 26.11 dropped x86_64-darwin, so advertising it here would hand
      # an Intel Mac user an evaluation throw instead of a binary.
      systems = [
        "aarch64-darwin"
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
              "-X github.com/kunchenguid/no-mistakes/internal/buildinfo.Commit=${commit}"
              "-X github.com/kunchenguid/no-mistakes/internal/buildinfo.Date=${date}"
              "-X github.com/kunchenguid/no-mistakes/internal/buildinfo.TelemetryWebsiteID=f959e889-92f5-4121-8a1f-571b10861198"
            ];
            # Parts of the suite need a real .git directory, an FHS /bin/bash,
            # and real-hardware wall-clock timing, none of which the hermetic
            # sandbox provides. Full regression runs in CI, not here.
            doCheck = false;
          };
        }
      );
    };
}
