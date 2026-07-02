package agent

import (
	"context"
	"fmt"
	"strings"
)

type fallbackAgent struct {
	agents []Agent
}

// NewFallback returns an Agent that tries each agent in order when an
// invocation fails because the current agent process is unavailable.
func NewFallback(agents []Agent) Agent {
	switch len(agents) {
	case 0:
		return nil
	case 1:
		return agents[0]
	default:
		copied := make([]Agent, len(agents))
		copy(copied, agents)
		return &fallbackAgent{agents: copied}
	}
}

func (a *fallbackAgent) Name() string {
	if len(a.agents) == 0 {
		return ""
	}
	return a.agents[0].Name()
}

func (a *fallbackAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	var lastErr error
	for i, current := range a.agents {
		result, err := current.Run(ctx, opts)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if i == len(a.agents)-1 || !isAgentUnavailableError(err) {
			return nil, err
		}
		next := a.agents[i+1]
		if opts.OnChunk != nil {
			opts.OnChunk(fmt.Sprintf("\nagent %s failed (%s); falling back to %s\n", current.Name(), fallbackReason(err), next.Name()))
		}
	}
	return nil, lastErr
}

func (a *fallbackAgent) Close() error {
	var errs []string
	for _, ag := range a.agents {
		if err := ag.Close(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", ag.Name(), err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("close fallback agents: %s", strings.Join(errs, "; "))
	}
	return nil
}

func isAgentUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	unavailable := []string{
		" start:",
		"start server ",
		" server: start server ",
		" exited:",
		" reported exit code ",
	}
	for _, needle := range unavailable {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func fallbackReason(err error) string {
	if err == nil {
		return "unknown error"
	}
	text := strings.Join(strings.Fields(err.Error()), " ")
	const max = 160
	if len([]rune(text)) <= max {
		return text
	}
	runes := []rune(text)
	return string(runes[:max]) + "..."
}
