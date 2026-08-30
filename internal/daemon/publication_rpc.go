package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/publication"
)

// publicationRPCService is the complete capability available to the public
// machine interface. It deliberately has no AXI, init, attach, scheduler, or
// direct external-effect method.
type publicationRPCService interface {
	Start(context.Context, publication.ParsedRequest) (publication.Result, error)
	Authorize(context.Context, publication.Authorization) (publication.Result, error)
	Status(context.Context, string) (publication.Result, error)
}

type publicationMutationGuard func(context.Context) error

// registerPublicationHandlers installs only the publication protocol surface.
// A nil service leaves the methods present but unavailable, so an ordinary
// daemon can fail this optional profile closed without exposing another path.
func registerPublicationHandlers(server *ipc.Server, service publicationRPCService, daemonIdentity ipc.PublicationIdentity, guard publicationMutationGuard, unavailableReasons ...error) {
	unavailable := fmt.Errorf("publication service is unavailable")
	if len(unavailableReasons) > 0 && unavailableReasons[0] != nil {
		unavailable = unavailableReasons[0]
	}
	server.Handle(ipc.MethodPublicationHandshake, func(_ context.Context, raw json.RawMessage) (any, error) {
		var params ipc.PublicationHandshakeParams
		if err := decodeCanonicalPublicationParams(raw, &params); err != nil {
			return nil, err
		}
		if service == nil {
			return nil, unavailable
		}
		if !validPublicationRPCIdentity(daemonIdentity) {
			return nil, fmt.Errorf("publication daemon identity is invalid")
		}
		if params.Identity != daemonIdentity {
			return nil, fmt.Errorf("publication CLI identity is incompatible with this daemon")
		}
		return &ipc.PublicationHandshakeResult{Identity: daemonIdentity}, nil
	})

	server.Handle(ipc.MethodPublicationStart, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var params ipc.PublicationStartParams
		if err := decodeCanonicalPublicationParams(raw, &params); err != nil {
			return nil, err
		}
		if service == nil {
			return nil, unavailable
		}
		if guard == nil {
			return nil, fmt.Errorf("publication mutation guard is unavailable")
		}
		if err := guard(ctx); err != nil {
			return nil, err
		}
		parsed, err := publication.ParseRequest(params.Request)
		if err != nil {
			return nil, err
		}
		if !publisherMatchesDaemon(parsed.Request.Publisher, daemonIdentity) {
			return nil, fmt.Errorf("publication request publisher does not match this daemon")
		}
		result, err := service.Start(ctx, parsed)
		if err != nil {
			return nil, err
		}
		return validatedPublicationRPCResult(result, parsed.PublicationID, parsed.Request.Candidate.CommitSHA)
	})

	server.Handle(ipc.MethodPublicationAuthorize, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var params ipc.PublicationAuthorizeParams
		if err := decodeCanonicalPublicationParams(raw, &params); err != nil {
			return nil, err
		}
		if service == nil {
			return nil, unavailable
		}
		if guard == nil {
			return nil, fmt.Errorf("publication mutation guard is unavailable")
		}
		if err := guard(ctx); err != nil {
			return nil, err
		}
		envelope, err := publication.ParseAuthorization(params.Authorization)
		if err != nil {
			return nil, err
		}
		result, err := service.Authorize(ctx, envelope.Authorization())
		if err != nil {
			return nil, err
		}
		return validatedPublicationRPCResult(result, envelope.PublicationID, envelope.CommitSHA)
	})

	server.Handle(ipc.MethodPublicationStatus, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var params ipc.PublicationStatusParams
		if err := decodeCanonicalPublicationParams(raw, &params); err != nil {
			return nil, err
		}
		if service == nil {
			return nil, unavailable
		}
		query, err := publication.ParseStatusQuery(params.Query)
		if err != nil {
			return nil, err
		}
		result, err := service.Status(ctx, query.PublicationID)
		if err != nil {
			return nil, err
		}
		return validatedPublicationRPCResult(result, query.PublicationID, "")
	})
}

func decodeCanonicalPublicationParams(raw []byte, dst any) error {
	if len(raw) == 0 {
		return fmt.Errorf("publication RPC params are empty")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("invalid publication RPC params: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("publication RPC params contain a trailing JSON value")
		}
		return fmt.Errorf("publication RPC params contain trailing input: %w", err)
	}
	canonical, err := json.Marshal(dst)
	if err != nil {
		return fmt.Errorf("marshal canonical publication RPC params: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return fmt.Errorf("publication RPC params are not canonical JSON")
	}
	return nil
}

func validatedPublicationRPCResult(result publication.Result, publicationID, headSHA string) (publication.Result, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return publication.Result{}, fmt.Errorf("marshal publication result: %w", err)
	}
	validated, err := publication.ParseResult(raw)
	if err != nil {
		return publication.Result{}, fmt.Errorf("publication service returned an invalid result: %w", err)
	}
	if validated.PublicationID != publicationID {
		return publication.Result{}, fmt.Errorf("publication service result is bound to another publication")
	}
	if headSHA != "" && validated.HeadSHA != headSHA {
		return publication.Result{}, fmt.Errorf("publication service result is bound to another candidate head")
	}
	return validated, nil
}

func publisherMatchesDaemon(binding publication.PublisherBinding, identity ipc.PublicationIdentity) bool {
	return validPublicationRPCIdentity(identity) &&
		binding.ExecutablePath == identity.ExecutablePath &&
		binding.ExecutableSHA256 == identity.ExecutableSHA256 &&
		binding.BuildSHA == identity.BuildSHA &&
		binding.Protocol == identity.Protocol
}

func validPublicationRPCIdentity(identity ipc.PublicationIdentity) bool {
	return isPortableAbsolutePath(identity.ExecutablePath) &&
		isLowerHexRPC(identity.ExecutableSHA256, 64) &&
		isLowerHexRPC(identity.BuildSHA, 40) &&
		identity.Protocol == publication.ProtocolV1
}

func isPortableAbsolutePath(value string) bool {
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, `\\`) {
		return true
	}
	return len(value) >= 3 &&
		((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) &&
		value[1] == ':' && (value[2] == '/' || value[2] == '\\')
}

func isLowerHexRPC(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
