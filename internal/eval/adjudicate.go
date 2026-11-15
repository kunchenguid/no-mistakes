package eval

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// LabelProvenance counts current labels once per case, independently of repeats.
type LabelProvenance struct {
	Inferred, RecordedHuman, Adjudicated, Excluded, Unknown, UnavailableCases int
	InferredShippedUnfixed                                                    int
}

func validateLabels(labels Labels) error {
	if labels.Version != labelsVersion {
		return fmt.Errorf("unsupported case labels version %d (finding-level gold replaced the park/pass verdict; remove the eval directory to start a fresh corpus)", labels.Version)
	}
	for i, g := range labels.Findings {
		if a := g.Adjudication; a != nil {
			if strings.TrimSpace(a.Reason) == "" || a.DecidedAt <= 0 || (a.Excluded && a.Kind != "") || (!a.Excluded && a.Kind != GoldTruePositive && a.Kind != GoldFalsePositive && a.Kind != GoldFalseNegative) {
				return fmt.Errorf("invalid adjudication at label index %d", i+1)
			}
		}
	}
	return nil
}

func (p *LabelProvenance) Add(labels Labels) {
	for _, g := range labels.Findings {
		switch g.GoldProvenance() {
		case "inferred":
			p.Inferred++
			if g.Source == goldSourceShippedUnfixed {
				p.InferredShippedUnfixed++
			}
		case "recorded-human":
			p.RecordedHuman++
		case "adjudicated":
			p.Adjudicated++
		case "excluded":
			p.Excluded++
		default:
			p.Unknown++
		}
	}
}

func (p LabelProvenance) String() string {
	text := fmt.Sprintf("inferred %d (shipped-unfixed %d), recorded-human %d, adjudicated %d, excluded %d, unknown %d", p.Inferred, p.InferredShippedUnfixed, p.RecordedHuman, p.Adjudicated, p.Excluded, p.Unknown)
	if p.UnavailableCases > 0 {
		text += fmt.Sprintf("; unavailable cases %d", p.UnavailableCases)
	}
	return text
}

// GoldProvenance distinguishes explicit adjudication from recorded decisions
// and inference. Older/externally curated sources remain unknown, never guessed.
func (g FindingGold) GoldProvenance() string {
	if g.Adjudication != nil {
		if g.Adjudication.Excluded {
			return "excluded"
		}
		return "adjudicated"
	}
	switch g.Source {
	case goldSourceShippedUnfixed, goldSourceAutoFixMerged, goldSourceCIFalseNegative:
		return "inferred"
	case goldSourceUserFix, goldSourceUserAdded, goldSourcePostPRMiss:
		return "recorded-human"
	default:
		return "unknown"
	}
}

// CaseByID resolves only a registered exact ID, never a caller-supplied path.
func (s *Store) CaseByID(id string) (Case, error) {
	if s == nil || s.db == nil {
		return Case{}, fmt.Errorf("eval registry is closed")
	}
	var dir string
	if err := s.db.QueryRow(`SELECT path FROM cases WHERE id = ?`, id).Scan(&dir); err != nil {
		return Case{}, fmt.Errorf("find eval case %q: %w", id, err)
	}
	return loadCase(dir)
}

// Adjudicate changes only local gold. Index is a one-based labels-file position
// for ID-less findings; a nonempty findingID must resolve uniquely instead.
// Capture, relabel, ingest and this writer share the corpus lock.
func (s *Store) Adjudicate(ctx context.Context, caseID, findingID string, index int, decision Adjudication) (Case, error) {
	if s == nil || s.db == nil {
		return Case{}, fmt.Errorf("eval registry is closed")
	}
	if (findingID == "") == (index == 0) || index < 0 {
		return Case{}, fmt.Errorf("select exactly one --finding ID or --index (one-based)")
	}
	decision.Reason = strings.TrimSpace(decision.Reason)
	if decision.Reason == "" {
		return Case{}, fmt.Errorf("adjudication requires a nonempty reason")
	}
	if decision.Excluded {
		if decision.Kind != "" {
			return Case{}, fmt.Errorf("--exclude and --kind are mutually exclusive")
		}
	} else if decision.Kind != GoldTruePositive && decision.Kind != GoldFalsePositive && decision.Kind != GoldFalseNegative {
		return Case{}, fmt.Errorf("kind must be true-positive, false-positive, or false-negative (or use --exclude)")
	}
	unlock, err := lockCorpus(ctx, s.root)
	if err != nil {
		return Case{}, err
	}
	defer unlock()
	c, err := s.CaseByID(caseID)
	if err != nil {
		return Case{}, err
	}
	selected := index - 1
	if findingID != "" {
		selected = -1
		for i, g := range c.Labels.Findings {
			if g.ID == findingID {
				if selected >= 0 {
					return Case{}, fmt.Errorf("finding ID %q is ambiguous; use --index", findingID)
				}
				selected = i
			}
		}
	}
	if selected < 0 || selected >= len(c.Labels.Findings) {
		return Case{}, fmt.Errorf("finding not found in case %q", caseID)
	}
	previous := c.Labels.Findings[selected].Adjudication
	if previous == nil || previous.Kind != decision.Kind || previous.Excluded != decision.Excluded || previous.Reason != decision.Reason {
		decision.DecidedAt = time.Now().Unix()
		c.Labels.Findings[selected].Adjudication = &decision
		if err := writeJSON(filepath.Join(c.Dir, "labels.json"), c.Labels); err != nil {
			return Case{}, fmt.Errorf("write adjudication: %w", err)
		}
	}
	if err := s.updateCaseGoldCount(c.ID, len(c.Labels.ActiveFindings())); err != nil {
		return Case{}, err
	}
	return c, nil
}
