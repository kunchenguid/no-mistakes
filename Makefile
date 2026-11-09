VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# Target platform: the host by default; cross-build just the platform you
# need, e.g. `make build GOOS=windows GOARCH=amd64`.
GOOS   ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
BIN    := bin/no-mistakes$(if $(filter windows,$(GOOS)),.exe)

# Telemetry stays unconfigured: no Umami host or website ID is embedded, and
# an empty config means the telemetry client never makes a network request.
LDFLAGS := -X github.com/kunchenguid/no-mistakes/internal/buildinfo.Version=$(VERSION) \
           -X github.com/kunchenguid/no-mistakes/internal/buildinfo.Commit=$(COMMIT) \
           -X github.com/kunchenguid/no-mistakes/internal/buildinfo.Date=$(DATE)

.PHONY: build install test e2e e2e-record lint fmt clean docs docs-build docs-preview demo skill skill-check winres winres-check

BINDIR ?= $(HOME)/.local/bin
INSTALL_BIN := $(BINDIR)/$(notdir $(BIN))

build:
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/no-mistakes

install: build
	mkdir -p $(BINDIR)
	install -m 755 $(BIN) $(INSTALL_BIN)

test:
	go test -race ./...

# End-to-end suite: drives the real no-mistakes binary against a fake
# agent through the full push -> pipeline -> push journey for each
# e2e-covered agent backend, plus the step-local e2e tests that live
# next to the pipeline-step code (e.g. coverage provider journeys).
# Excluded from `make test` because it is behind the `e2e` build tag and
# rebuilds binaries on each run.
#
# scripts/e2e.sh owns temporary-daemon inventory + EXIT/INT/TERM reaping so
# an interrupted or timed-out go test child cannot leave detached e2e
# daemons behind. Keepalive shells are out of scope. A SIGKILL of the
# wrapper shell itself does not run its trap; next-run pre-reap recovers.
e2e:
	@bash scripts/e2e.sh

# Re-record fixtures from the real claude/codex/opencode/antigravity CLIs and overwrite
# internal/e2e/fixtures/. Spends real API quota — run only when the upstream
# wire format changes or when adding a new agent or flavour. Personal paths are
# scrubbed automatically; review the diff before committing.
e2e-record:
	go run ./cmd/recordfixture claude   --out internal/e2e/fixtures/claude
	go run ./cmd/recordfixture codex    --out internal/e2e/fixtures/codex
	go run ./cmd/recordfixture opencode --out internal/e2e/fixtures/opencode
	go run ./cmd/recordfixture antigravity --out internal/e2e/fixtures/antigravity

# Regenerate the committed agent skill (skills/no-mistakes/SKILL.md) from the
# internal/skill source of truth.
skill:
	go run ./cmd/genskill

# Fail if the committed skill has drifted from the generator. Wired into lint
# so a forgotten `make skill` is caught.
skill-check:
	go run ./cmd/genskill --check

# Regenerate the committed Windows resource objects
# (cmd/no-mistakes/rsrc_windows_*.syso) that embed the application manifest
# from internal/winres.
winres:
	go run ./cmd/genwinres

# Fail if a committed resource object has drifted from the generator. Wired
# into lint alongside skill-check.
winres-check:
	go run ./cmd/genwinres --check

lint: skill-check winres-check
	go vet ./...

fmt:
	gofmt -w .

docs: docs-build

docs-build:
	cd docs && npm ci && npm run build

docs-preview:
	cd docs && npm run preview

demo: build
	vhs demo.tape
	ffmpeg -i demo_raw.gif -filter_complex "\
		[0:v]split[orig][zoom_src];\
		[zoom_src]crop=963:570:0:0,scale=1100:650:flags=lanczos[zoomed];\
		[orig]scale=1100:650:flags=lanczos[base];\
		[base][zoomed]overlay=0:0:enable='lt(t,4.04)',setpts=1.9*PTS,\
		split[s0][s1];\
		[s0]palettegen=max_colors=128[p];\
		[s1][p]paletteuse=dither=sierra2_4a\
	" -r 10 -y demo.gif
	ffmpeg -i demo_raw.gif -filter_complex "\
		[0:v]split[orig][zoom_src];\
		[zoom_src]crop=963:570:0:0,scale=1100:650:flags=lanczos[zoomed];\
		[orig]scale=1100:650:flags=lanczos[base];\
		[base][zoomed]overlay=0:0:enable='lt(t,4.04)',setpts=1.9*PTS\
	" -c:v libx264 -pix_fmt yuv420p -movflags +faststart -r 30 -y demo.mp4
	rm -f demo_raw.gif

clean:
	rm -rf bin/
