//go:build factorypublicatione2e

package daemon

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/publication"
)

const factoryPublicationE2EAPIEnv = "NM_E2E_FACTORY_PUBLICATION_API"

type offlineE2ETestOnlyUnconfinedPublicationDefenseBoundary struct{}

func (offlineE2ETestOnlyUnconfinedPublicationDefenseBoundary) publicationDefenseBoundary() {}

func init() {
	overrideProductionPublicationComposition = newOfflineE2EPublicationComposition
}

// newOfflineE2EPublicationComposition is compiled only into the dedicated
// offline test binary. It proves the core composition and external-effect
// contract with an explicitly unconfined test boundary; it does not prove or
// emulate production confinement. With no endpoint it declines the override.
func newOfflineE2EPublicationComposition(
	p *paths.Paths,
	database *db.DB,
	runs *RunManager,
	global *config.GlobalConfig,
	identity publication.PublisherBinding,
) (*publicationComposition, bool, error) {
	endpoint := strings.TrimSpace(os.Getenv(factoryPublicationE2EAPIEnv))
	if endpoint == "" {
		return nil, false, nil
	}
	push, err := publication.NewGitPushPort(publication.GitPushPortOptions{DB: database})
	if err != nil {
		return nil, true, fmt.Errorf("compose offline E2E Push port: %w", err)
	}
	github, err := publication.NewGitHubV1Port(publication.GitHubV1PortOptions{
		DB: database,
		HTTPClient: &http.Client{
			Timeout: 15 * time.Second,
		},
		APIBaseURL: endpoint,
	})
	if err != nil {
		return nil, true, fmt.Errorf("compose offline E2E GitHub ports: %w", err)
	}
	composition, err := newPublicationComposition(publicationCompositionOptions{
		Paths: p, DB: database, Runs: runs, GlobalConfig: global, Identity: identity,
		TestOnlyUnconfinedDefenseBoundary: offlineE2ETestOnlyUnconfinedPublicationDefenseBoundary{},
		Push:                              push, PR: github, CI: github,
	})
	return composition, true, err
}
