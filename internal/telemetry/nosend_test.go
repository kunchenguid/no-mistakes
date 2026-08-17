package telemetry

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
)

// This fork never sends telemetry: Default() is a no-op sink at the source.
// The test sets every knob upstream uses to build a live sender - runtime env,
// repo dotenv, and build-time embedded values - and still requires a no-op.
func TestDefaultStaysNoopWithFullTelemetryConfiguration(t *testing.T) {
	prevSink := defaultSink
	defaultSink = nil
	defer func() { defaultSink = prevSink }()

	prevHost := buildinfo.TelemetryHost
	prevVersion := buildinfo.Version
	prevWebsiteID := buildinfo.TelemetryWebsiteID
	defer func() {
		buildinfo.TelemetryHost = prevHost
		buildinfo.Version = prevVersion
		buildinfo.TelemetryWebsiteID = prevWebsiteID
	}()
	buildinfo.TelemetryHost = "https://embedded.example"
	buildinfo.Version = "v1.2.3"
	buildinfo.TelemetryWebsiteID = "embedded-website"

	t.Setenv(telemetryEnv, "on")
	t.Setenv(umamiHostEnv, "https://env.example")
	t.Setenv(umamiWebsiteIDEnv, "website-from-env")

	if defaultWebsiteID() == "" || defaultHostValue() == "" {
		t.Fatal("test setup resolves no collector config, so a no-op sink would prove nothing")
	}

	sink := Default()
	if _, ok := sink.(noopSink); !ok {
		t.Fatalf("Default() type = %T, want noopSink", sink)
	}
	if Enabled() {
		t.Fatal("Enabled() = true, want false")
	}

	Track("command", Fields{"command": "status"})
	Pageview("/tui", nil)
}

// SetDefaultForTesting is the one way to install another sink, so call-site
// tests can still record the events the pipeline reports.
func TestSetDefaultForTestingStillInstallsAndRestoresASink(t *testing.T) {
	recorder := &recordingSink{}

	restore := SetDefaultForTesting(recorder)
	Track("command", Fields{"command": "status"})
	if recorder.tracked != 1 {
		t.Fatalf("tracked = %d, want 1", recorder.tracked)
	}

	restore()
	if _, ok := Default().(noopSink); !ok {
		t.Fatalf("Default() after restore = %T, want noopSink", Default())
	}
}

type recordingSink struct {
	tracked int
}

func (s *recordingSink) Track(string, Fields) { s.tracked++ }

func (s *recordingSink) Pageview(string, Fields) {}

func (s *recordingSink) Close(context.Context) error { return nil }
