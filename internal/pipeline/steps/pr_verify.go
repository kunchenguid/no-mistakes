package steps

import (
	"context"
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/forgejo"
)

// PRAppendixVerification reports the publication parser's ownership decision.
// A matching digest is an accidental-edit guard, not authentication or proof
// that the attestation describes the PR's current head.
type PRAppendixVerification struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

// VerifyPRAppendix never changes the supplied body. Publication remains the
// sole owner of marker parsing and legacy digest recovery.
func VerifyPRAppendix(body string) PRAppendixVerification {
	parts, err := parsePROwnedBody(body)
	switch {
	case !hasPRAppendixMarkers(body):
		reason := "no owned PR appendix is present"
		if err != nil {
			reason = err.Error()
		}
		return PRAppendixVerification{"absent", reason}
	case errors.Is(err, errPRAppendixMismatch):
		return PRAppendixVerification{"mismatch", err.Error()}
	case err != nil:
		return PRAppendixVerification{"malformed", err.Error()}
	case parts.managed:
		return PRAppendixVerification{"match", "owned PR appendix accepted by the publication parser"}
	default:
		return PRAppendixVerification{"absent", "no owned PR appendix is present"}
	}
}

type PRAppendixReader func(context.Context, string) (scm.PRContent, error)

// NewPRAppendixReader uses the publication provider adapters without opening a
// run, database, or daemon connection. Only the read interface escapes.
func NewPRAppendixReader(ctx context.Context, cfg *config.Config, repoURL string) (PRAppendixReader, error) {
	forge, err := forgecontext.Resolve(ctx, cfg.ForgeProfiles, repoURL, "")
	if err != nil {
		return nil, err
	}
	if err := forgecontext.RefuseProviderPluginOverlap(ctx, forge, cfg.ProviderPlugins, repoURL); err != nil {
		return nil, err
	}
	sctx := &pipeline.StepContext{
		Ctx: ctx, Config: cfg, ForgeContext: forge,
		Repo: &db.Repo{UpstreamURL: repoURL}, Run: &db.Run{},
	}
	host, reason := buildHost(sctx, resolvedProvider(sctx))
	if host == nil {
		return nil, fmt.Errorf("cannot read PR content: %s", reason)
	}
	if err := host.Available(ctx); err != nil {
		return nil, fmt.Errorf("cannot read PR content: %w", err)
	}
	if host, ok := host.(*forgejo.Host); ok {
		return host.GetPRContentByNumber, nil
	}
	reader, ok := host.(scm.PRContentReader)
	if !ok {
		return nil, fmt.Errorf("provider does not support reading PR content")
	}
	return func(ctx context.Context, number string) (scm.PRContent, error) {
		return reader.GetPRContent(ctx, &scm.PR{Number: number})
	}, nil
}
