{
  lib,
  buildGoModule,
  version,
  commit ? "unknown",
  date ? "unknown",
}:

let
  buildinfo = "github.com/kunchenguid/no-mistakes/internal/buildinfo";
in
buildGoModule {
  pname = "no-mistakes";
  inherit version;

  src = lib.cleanSource ./.;
  vendorHash = "sha256-maAVBptEtdrGanJHwAPAmuGBorzIMUgK6T+NmIz1kS0=";
  subPackages = [ "cmd/no-mistakes" ];

  env.CGO_ENABLED = 0;
  ldflags = [
    "-X ${buildinfo}.Version=v${version}"
    "-X ${buildinfo}.Commit=${commit}"
    "-X ${buildinfo}.Date=${date}"
    "-X ${buildinfo}.TelemetryHost=https://a.kunchenguid.com"
    "-X ${buildinfo}.TelemetryWebsiteID=f959e889-92f5-4121-8a1f-571b10861198"
  ];

  # The suite needs a real .git directory, /bin/bash, and wall-clock timing
  # the build sandbox does not provide; ci.yml runs it on three OSes instead.
  doCheck = false;

  meta = {
    description = "Local gate that reviews, tests, and fixes every push before it ships";
    homepage = "https://github.com/kunchenguid/no-mistakes";
    license = lib.licenses.mit;
    mainProgram = "no-mistakes";
  };
}
