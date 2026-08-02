package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/scm/gitlab"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// StepFactory creates pipeline steps for a run. Defaults to steps.AllSteps.
type StepFactory func() []pipeline.Step

var recoveredConfigFetchTimeout = 10 * time.Second

var fetchRecoveredRemoteBranch = git.FetchRemoteBranch

type publicationCutoffsContextKey struct{}

func withPublicationCutoffs(ctx context.Context, cutoffs map[string]int64) context.Context {
	copyCutoffs := make(map[string]int64, len(cutoffs))
	for fingerprint, cutoff := range cutoffs {
		copyCutoffs[fingerprint] = cutoff
	}
	return context.WithValue(ctx, publicationCutoffsContextKey{}, copyCutoffs)
}

func publicationCutoff(ctx context.Context, fingerprint string) int64 {
	cutoffs, _ := ctx.Value(publicationCutoffsContextKey{}).(map[string]int64)
	return cutoffs[fingerprint]
}

func publicationCutoffsFromEvidence(evidence map[string]scm.HistoricalPublicationEvidence) (map[string]int64, error) {
	cutoffs := make(map[string]int64, len(evidence))
	for fingerprint, proof := range evidence {
		if strings.TrimSpace(proof.HighWater) == "local-snapshot" {
			continue
		}
		value := strings.TrimPrefix(strings.TrimSpace(proof.HighWater), "provider-date:")
		cutoff, err := strconv.ParseInt(value, 10, 64)
		if err != nil || cutoff <= 0 {
			return nil, fmt.Errorf("publication evidence for %s has no provider-issued cutoff", fingerprint)
		}
		cutoffs[fingerprint] = cutoff
	}
	return cutoffs, nil
}

// RunManager tracks active pipeline executors and manages run lifecycle.
type RunManager struct {
	mu           sync.Mutex
	executors    map[string]*pipeline.Executor      // runID → executor
	cancels      map[string]context.CancelCauseFunc // runID → cancel function with cause
	dones        map[string]chan struct{}           // runID → closed when goroutine exits
	wg           sync.WaitGroup                     // tracks background run goroutines
	shuttingDown atomic.Bool                        // prevents new runs during shutdown
	db           *db.DB
	paths        *paths.Paths
	steps        StepFactory

	branchLocks sync.Map // repoID+"/"+branch → *sync.Mutex

	managedGateMu                  sync.Mutex
	managedGateGuards              map[string]*branchsync.ManagedGateRefAuthority
	managedGateQuarantine          map[string]error
	managedGateQuarantinePersisted map[string]bool
	quarantineGateRef              func(string, string, string, string, string, string) error
	recoveryAnchorMu               sync.Mutex

	// subMu guards the subscriber set and the per-run state revisions. It is
	// a plain Mutex, not an RWMutex, because revision assignment and fan-out
	// must be one atomic step: if two concurrent state events could be
	// enqueued out of revision order, a consumer's monotonic guard would
	// permanently discard the older one's payload. The critical section
	// contains no blocking operation and no I/O, so hold time is
	// O(subscribers) memory writes.
	subMu          sync.Mutex
	subscribers    map[string][]*eventMailbox // runID → subscriber mailboxes
	stateRevs      map[string]int64           // runID → monotonic state revision
	completedRuns  map[string]bool            // runIDs whose goroutines have finished
	completedOrder []string                   // insertion order for FIFO eviction
}

// maxSubscribersPerRun bounds the global mailbox footprint: queued bytes can
// never exceed activeRuns × maxSubscribersPerRun × mailboxMaxBytes. Refusing
// past the cap is an ordinary error, never unbounded growth.
const maxSubscribersPerRun = 32

// NewRunManager creates a RunManager. Pass nil for stepFactory to use default steps.
func NewRunManager(database *db.DB, p *paths.Paths, stepFactory StepFactory) *RunManager {
	if stepFactory == nil {
		stepFactory = func() []pipeline.Step { return steps.AllSteps() }
	}
	return &RunManager{
		executors:                      make(map[string]*pipeline.Executor),
		cancels:                        make(map[string]context.CancelCauseFunc),
		dones:                          make(map[string]chan struct{}),
		db:                             database,
		paths:                          p,
		steps:                          stepFactory,
		subscribers:                    make(map[string][]*eventMailbox),
		stateRevs:                      make(map[string]int64),
		completedRuns:                  make(map[string]bool),
		managedGateGuards:              make(map[string]*branchsync.ManagedGateRefAuthority),
		managedGateQuarantine:          make(map[string]error),
		managedGateQuarantinePersisted: make(map[string]bool),
		quarantineGateRef: func(repoID, gatePath, ref, expected, observed, reason string) error {
			return database.QuarantineGateRef(repoID, gatePath, ref, expected, observed, reason)
		},
	}
}

func managedGateGuardKey(repoID, ref string) string {
	return strings.TrimSpace(repoID) + "\x00" + strings.TrimSpace(ref)
}

func (m *RunManager) ensureManagedGateGuard(repo *db.Repo, ref string) error {
	if m == nil || m.db == nil || repo == nil || !strings.HasPrefix(ref, "refs/heads/") {
		return nil
	}
	key := managedGateGuardKey(repo.ID, ref)
	m.managedGateMu.Lock()
	defer m.managedGateMu.Unlock()
	if cause, ok := m.managedGateQuarantine[key]; ok {
		quarantine, err := m.db.GetGateRefQuarantine(repo.ID, m.paths.RepoDir(repo.ID), ref)
		if err != nil || quarantine != nil || !m.managedGateQuarantinePersisted[key] {
			return fmt.Errorf("managed gate authority remains quarantined: %w", cause)
		}
		delete(m.managedGateQuarantine, key)
	}
	if err := m.ensureManagedGateRefAvailable(repo, ref); err != nil {
		return err
	}
	if guard, ok := m.managedGateGuards[key]; ok {
		if err := m.validateManagedGateGuardLocked(repo.ID, m.paths.RepoDir(repo.ID), ref, guard); err != nil {
			return err
		}
		return nil
	}
	guard, err := branchsync.AcquireManagedGateRefAuthority(m.paths.RepoDir(repo.ID), ref)
	if err != nil {
		return err
	}
	if err := m.validateManagedGateGuardLocked(repo.ID, m.paths.RepoDir(repo.ID), ref, guard); err != nil {
		_ = guard.Invalidate()
		return err
	}
	m.managedGateGuards[key] = guard
	return nil
}

func (m *RunManager) reconcileManagedGateQuarantine(ctx context.Context, repo *db.Repo, ref string) error {
	if m == nil || m.db == nil || repo == nil || !strings.HasPrefix(ref, "refs/heads/") {
		return nil
	}
	gateDir := m.paths.RepoDir(repo.ID)
	quarantine, err := m.db.GetGateRefQuarantine(repo.ID, gateDir, ref)
	if err != nil || quarantine == nil {
		return err
	}
	ownership, err := branchsync.AcquireBranchOwnershipLock(m.paths, repo, repo.WorkingPath, branchFromRef(ref))
	if err != nil {
		return fmt.Errorf("acquire branch ownership lock for quarantine reconciliation: %w", err)
	}
	defer ownership.Release()
	m.managedGateMu.Lock()
	defer m.managedGateMu.Unlock()
	key := managedGateGuardKey(repo.ID, ref)
	guard := m.managedGateGuards[key]
	ownedHere := guard != nil
	if guard == nil {
		guard, err = branchsync.AcquireManagedGateRefAuthority(gateDir, ref)
		if err != nil {
			return fmt.Errorf("acquire managed gate authority for quarantine reconciliation: %w", err)
		}
	} else if err := guard.Validate(gateDir, ref); err != nil {
		if invalidateErr := guard.Invalidate(); invalidateErr != nil {
			return fmt.Errorf("invalidate stale managed gate authority for quarantine reconciliation: %w", invalidateErr)
		}
		delete(m.managedGateGuards, key)
		ownedHere = false
		guard, err = branchsync.AcquireManagedGateRefAuthority(gateDir, ref)
		if err != nil {
			return fmt.Errorf("reacquire managed gate authority for quarantine reconciliation: %w", err)
		}
	}
	current, readErr := branchsync.ReadManagedGateRefUnderAuthority(guard, gateDir, ref)
	if readErr != nil || db.NormalizeManagedGateHead(current) != db.NormalizeManagedGateHead(quarantine.ExpectedHead) {
		if !ownedHere {
			_ = guard.Release()
		}
		if readErr != nil {
			return fmt.Errorf("read quarantined managed gate ref: %w", readErr)
		}
		return fmt.Errorf("quarantined managed gate ref changed from expected %s to %s", quarantine.ExpectedHead, current)
	}
	if err := m.db.ClearGateRefQuarantine(repo.ID, gateDir, ref); err != nil {
		if !ownedHere {
			_ = guard.Release()
		}
		return fmt.Errorf("clear reconciled managed gate quarantine: %w", err)
	}
	if !ownedHere {
		m.managedGateGuards[key] = guard
	}
	delete(m.managedGateQuarantine, key)
	delete(m.managedGateQuarantinePersisted, key)
	_ = ctx
	return nil
}

// acquireManagedGateGuardLocked returns the live authority while the caller
// holds managedGateMu. It deliberately does not validate the database head;
// recovery may need to inspect a quarantined projection before it can prove
// whether the journal can be advanced.
func (m *RunManager) acquireManagedGateGuardLocked(repoID, gateDir, ref string) (*branchsync.ManagedGateRefAuthority, bool, error) {
	key := managedGateGuardKey(repoID, ref)
	guard := m.managedGateGuards[key]
	ownedHere := false
	if guard != nil {
		if err := guard.Validate(gateDir, ref); err != nil {
			if invalidateErr := guard.Invalidate(); invalidateErr != nil {
				return nil, false, fmt.Errorf("invalidate stale managed gate authority: %w", invalidateErr)
			}
			delete(m.managedGateGuards, key)
			guard = nil
		}
	}
	if guard == nil {
		var err error
		guard, err = branchsync.AcquireManagedGateRefAuthority(gateDir, ref)
		if err != nil {
			return nil, false, err
		}
		ownedHere = true
	}
	return guard, ownedHere, nil
}

// reconcileRecoveryManagedGateGuardLocked advances a quarantined managed-head
// journal only when the live ref is exactly the caller's already-verified old
// head. The custody service holds the branch and recovery locks while calling
// this function, so the journal update remains part of that transition.
func (m *RunManager) reconcileRecoveryManagedGateGuardLocked(repoID, gateDir, ref, expected string, guard *branchsync.ManagedGateRefAuthority) error {
	current, err := branchsync.ReadManagedGateRefUnderAuthority(guard, gateDir, ref)
	if err != nil {
		return err
	}
	if strings.TrimSpace(expected) != "" && db.NormalizeManagedGateHead(current) != db.NormalizeManagedGateHead(expected) {
		return fmt.Errorf("managed gate ref changed from expected %s to %s", expected, current)
	}
	quarantine, err := m.db.GetGateRefQuarantine(repoID, gateDir, ref)
	if err != nil {
		return err
	}
	if quarantine != nil {
		if db.NormalizeManagedGateHead(current) == db.NormalizeManagedGateHead(quarantine.ExpectedHead) {
			if err := m.db.ClearGateRefQuarantine(repoID, gateDir, ref); err != nil {
				return err
			}
		} else {
			if db.NormalizeManagedGateHead(current) != db.NormalizeManagedGateHead(quarantine.ObservedHead) {
				return fmt.Errorf("quarantined managed gate ref changed from observed %s to %s", quarantine.ObservedHead, current)
			}
			managed, managedErr := m.db.GetManagedGateRef(repoID, gateDir, ref)
			if managedErr != nil {
				return managedErr
			}
			if managed == nil || db.NormalizeManagedGateHead(managed.Head) != db.NormalizeManagedGateHead(quarantine.ExpectedHead) {
				return fmt.Errorf("quarantined managed gate ref has no matching journaled head")
			}
			if err := m.db.SetManagedGateRefHead(repoID, gateDir, ref, current); err != nil {
				return err
			}
			if err := m.db.ClearGateRefQuarantine(repoID, gateDir, ref); err != nil {
				return err
			}
		}
		key := managedGateGuardKey(repoID, ref)
		delete(m.managedGateQuarantine, key)
		delete(m.managedGateQuarantinePersisted, key)
	}
	return m.validateManagedGateGuardLocked(repoID, gateDir, ref, guard)
}

func (m *RunManager) managedGateRefMutation(repoID, gateDir string) func(context.Context, string, string, string, func(string, string) error, func() error) error {
	return func(ctx context.Context, ref, oldSHA, newSHA string, prepare func(string, string) error, commit func() error) error {
		m.managedGateMu.Lock()
		guard, ownedHere, err := m.acquireManagedGateGuardLocked(repoID, gateDir, ref)
		if err != nil {
			m.managedGateMu.Unlock()
			return err
		}
		if err := m.reconcileRecoveryManagedGateGuardLocked(repoID, gateDir, ref, oldSHA, guard); err != nil {
			if ownedHere {
				_ = guard.Release()
			}
			m.managedGateMu.Unlock()
			return err
		}
		if err := prepare(guard.Path(), guard.Identity()); err != nil {
			if ownedHere {
				_ = guard.Release()
			}
			m.managedGateMu.Unlock()
			return err
		}
		if err := guard.UpdateRef(ctx, gateDir, ref, oldSHA, newSHA); err != nil {
			if ownedHere {
				_ = guard.Release()
			}
			m.managedGateMu.Unlock()
			return err
		}
		if ownedHere {
			m.managedGateGuards[managedGateGuardKey(repoID, ref)] = guard
		}
		// Keep the authority file held across commit, but release the Go mutex:
		// commit re-enters managedGateRefRead.
		m.managedGateMu.Unlock()
		return commit()
	}
}

func (m *RunManager) managedGateRefRead(repoID, gateDir, ref string) func(string) (string, error) {
	return func(requestedRef string) (string, error) {
		if requestedRef != ref {
			return "", fmt.Errorf("managed gate authority ref mismatch")
		}
		m.managedGateMu.Lock()
		guard, ownedHere, err := m.acquireManagedGateGuardLocked(repoID, gateDir, ref)
		if err != nil {
			m.managedGateMu.Unlock()
			return "", err
		}
		if ownedHere {
			m.managedGateGuards[managedGateGuardKey(repoID, ref)] = guard
		}
		current, readErr := branchsync.ReadManagedGateRefUnderAuthority(guard, gateDir, ref)
		if readErr != nil && ownedHere {
			_ = guard.Release()
		}
		m.managedGateMu.Unlock()
		return current, readErr
	}
}

func (m *RunManager) managedGateRefFinalize(repoID, gateDir, ref string) func(context.Context, string, string, func() error, func() error) error {
	return func(ctx context.Context, requestedRef, expected string, stamp func() error, rollback func() error) error {
		if requestedRef != ref {
			return fmt.Errorf("managed gate authority ref mismatch")
		}
		m.managedGateMu.Lock()
		guard, ownedHere, err := m.acquireManagedGateGuardLocked(repoID, gateDir, ref)
		if err != nil {
			m.managedGateMu.Unlock()
			return err
		}
		if err := m.reconcileRecoveryManagedGateGuardLocked(repoID, gateDir, ref, expected, guard); err != nil {
			if ownedHere {
				_ = guard.Release()
			}
			m.managedGateMu.Unlock()
			return err
		}
		current, err := branchsync.ReadManagedGateRefUnderAuthority(guard, gateDir, ref)
		if err != nil {
			if ownedHere {
				_ = guard.Release()
			}
			m.managedGateMu.Unlock()
			return err
		}
		if db.NormalizeManagedGateHead(current) != db.NormalizeManagedGateHead(expected) {
			if ownedHere {
				_ = guard.Release()
			}
			m.managedGateMu.Unlock()
			return fmt.Errorf("managed gate ref changed during final custody check")
		}
		if ownedHere {
			m.managedGateGuards[managedGateGuardKey(repoID, ref)] = guard
		}
		if err := stamp(); err != nil {
			if rollbackErr := rollback(); rollbackErr != nil {
				m.managedGateMu.Unlock()
				return fmt.Errorf("managed custody stamp failed: %w; rollback failed: %v", err, rollbackErr)
			}
			m.managedGateMu.Unlock()
			return err
		}
		if err := m.validateManagedGateGuardLocked(repoID, gateDir, ref, guard); err != nil {
			if rollbackErr := rollback(); rollbackErr != nil {
				m.managedGateMu.Unlock()
				return fmt.Errorf("managed gate authority was lost after custody stamp: %w; rollback failed: %v", err, rollbackErr)
			}
			m.managedGateMu.Unlock()
			return fmt.Errorf("managed gate authority was lost after custody stamp: %w", err)
		}
		current, err = branchsync.ReadManagedGateRefUnderAuthority(guard, gateDir, ref)
		if err != nil {
			if rollbackErr := rollback(); rollbackErr != nil {
				m.managedGateMu.Unlock()
				return fmt.Errorf("managed gate ref could not be re-read after custody stamp: %w; rollback failed: %v", err, rollbackErr)
			}
			m.managedGateMu.Unlock()
			return fmt.Errorf("managed gate ref could not be re-read after custody stamp: %w", err)
		}
		if db.NormalizeManagedGateHead(current) != db.NormalizeManagedGateHead(expected) {
			if rollbackErr := rollback(); rollbackErr != nil {
				m.managedGateMu.Unlock()
				return fmt.Errorf("managed gate ref changed after final custody stamp; rollback failed: %v", rollbackErr)
			}
			m.managedGateMu.Unlock()
			return fmt.Errorf("managed gate ref changed after final custody stamp")
		}
		_ = ctx
		m.managedGateMu.Unlock()
		return nil
	}
}

func (m *RunManager) managedPrivateRefMutation(ctx context.Context, ref, oldSHA, newSHA string, write func(context.Context) error) error {
	if strings.TrimSpace(ref) == "" || strings.TrimSpace(oldSHA) == "" || strings.TrimSpace(newSHA) == "" {
		return fmt.Errorf("private recovery ref mutation requires exact identity")
	}
	m.recoveryAnchorMu.Lock()
	defer m.recoveryAnchorMu.Unlock()
	return write(ctx)
}

func (m *RunManager) validateManagedGateGuardLocked(repoID, gateDir, ref string, guard *branchsync.ManagedGateRefAuthority) error {
	if err := guard.Validate(gateDir, ref); err != nil {
		repo, repoErr := m.db.GetRepo(repoID)
		if repoErr == nil && repo != nil {
			if quarantineErr := m.quarantineManagedGateGuardLocked(repo, ref, err); quarantineErr != nil {
				return quarantineErr
			}
		}
		return fmt.Errorf("managed gate authority is no longer valid: %w", err)
	}
	managed, err := m.db.GetManagedGateRef(repoID, gateDir, ref)
	if err != nil {
		return fmt.Errorf("read managed gate authority ledger: %w", err)
	}
	if managed == nil {
		cause := fmt.Errorf("managed gate authority ledger is missing for %s", ref)
		repo, repoErr := m.db.GetRepo(repoID)
		if repoErr != nil || repo == nil {
			return cause
		}
		if quarantineErr := m.quarantineManagedGateGuardLocked(repo, ref, cause); quarantineErr != nil {
			return quarantineErr
		}
		return cause
	}
	current, err := branchsync.ReadManagedGateRefUnderAuthority(guard, gateDir, ref)
	if err != nil {
		cause := fmt.Errorf("read managed gate ref projection: %w", err)
		repo, repoErr := m.db.GetRepo(repoID)
		if repoErr != nil || repo == nil {
			return cause
		}
		if quarantineErr := m.quarantineManagedGateGuardLocked(repo, ref, cause); quarantineErr != nil {
			return quarantineErr
		}
		return cause
	}
	if db.NormalizeManagedGateHead(current) != db.NormalizeManagedGateHead(managed.Head) {
		cause := fmt.Errorf("managed gate ref changed from journaled head %s to %s", managed.Head, current)
		repo, repoErr := m.db.GetRepo(repoID)
		if repoErr != nil || repo == nil {
			return cause
		}
		if quarantineErr := m.quarantineManagedGateGuardLocked(repo, ref, cause); quarantineErr != nil {
			return quarantineErr
		}
		return cause
	}
	return nil
}

func (m *RunManager) quarantineManagedGateGuardLocked(repo *db.Repo, ref string, cause error) error {
	if repo == nil {
		return fmt.Errorf("managed gate quarantine requires a repository")
	}
	gateDir := m.paths.RepoDir(repo.ID)
	managed, _ := m.db.GetManagedGateRef(repo.ID, gateDir, ref)
	expected := ""
	if managed != nil {
		expected = managed.Head
	}
	observed, observeErr := git.Run(context.Background(), gateDir, "rev-parse", ref+"^{commit}")
	if observeErr != nil {
		observed = ""
	}
	key := managedGateGuardKey(repo.ID, ref)
	if m.managedGateQuarantine == nil {
		m.managedGateQuarantine = make(map[string]error)
	}
	if m.managedGateQuarantinePersisted == nil {
		m.managedGateQuarantinePersisted = make(map[string]bool)
	}
	m.managedGateQuarantine[key] = cause
	quarantine := m.quarantineGateRef
	if quarantine == nil {
		quarantine = m.db.QuarantineGateRef
	}
	persistErr := quarantine(repo.ID, gateDir, ref, expected, observed, "managed-gate-authority-lost: "+cause.Error())
	m.managedGateQuarantinePersisted[key] = persistErr == nil
	if guard := m.managedGateGuards[key]; guard != nil {
		_ = guard.Invalidate()
		delete(m.managedGateGuards, key)
	}
	if persistErr != nil {
		return fmt.Errorf("managed gate authority quarantine could not be persisted: %w", persistErr)
	}
	return nil
}

func (m *RunManager) releaseManagedGateGuard(repoID, ref string) error {
	if m == nil {
		return nil
	}
	key := managedGateGuardKey(repoID, ref)
	m.managedGateMu.Lock()
	defer m.managedGateMu.Unlock()
	guard := m.managedGateGuards[key]
	if guard == nil {
		return nil
	}
	if err := guard.Release(); err != nil {
		return err
	}
	delete(m.managedGateGuards, key)
	return nil
}

func (m *RunManager) pipelineBranchRefUpdater(repo *db.Repo) pipeline.BranchRefUpdater {
	return func(ctx context.Context, ref, oldSHA, newSHA string) error {
		if repo == nil || !strings.HasPrefix(ref, "refs/heads/") {
			return fmt.Errorf("pipeline branch ref update requires an ordinary managed ref")
		}
		oldSHA = strings.TrimSpace(oldSHA)
		newSHA = strings.TrimSpace(newSHA)
		if oldSHA == "" || newSHA == "" {
			return fmt.Errorf("pipeline branch ref update requires exact old and new heads")
		}
		branch := branchFromRef(ref)
		lock, err := branchsync.AcquireBranchOwnershipLock(m.paths, repo, repo.WorkingPath, branch)
		if err != nil {
			return fmt.Errorf("acquire branch ownership lock for pipeline ref update: %w", err)
		}
		defer lock.Release()
		gateDir := m.paths.RepoDir(repo.ID)
		capability, authority, err := branchsync.IssueInternalRefMutation(m.db, lock, db.InternalRefMutationSpec{
			RepoID: repo.ID, GatePath: gateDir, Branch: branch, Ref: ref,
			OldSHA: oldSHA, NewSHA: newSHA, Operation: "update-ref", Scope: db.InternalRefMutationScopeOrdinary,
		})
		if err != nil {
			return err
		}
		mutationCtx := git.WithInternalMutationCapability(ctx, capability)
		mutationCtx = git.WithInternalMutationOperation(mutationCtx, "update-ref")
		mutationCtx = git.WithInternalMutationBranch(mutationCtx, branch)
		mutationCtx = git.WithInternalMutationAuthority(mutationCtx, authority)
		if _, err := git.Run(mutationCtx, gateDir, "update-ref", ref, newSHA, oldSHA); err != nil {
			return err
		}
		mutation, err := m.db.GetInternalRefMutation(capability)
		if err != nil {
			return err
		}
		if mutation.State != db.InternalRefMutationStateConsumed {
			return fmt.Errorf("pipeline branch ref update was not authorized")
		}
		return m.db.AdvanceManagedGateRefHead(repo.ID, gateDir, ref, oldSHA, newSHA)
	}
}

func (m *RunManager) restoreManagedGateGuards() error {
	if m == nil || m.db == nil {
		return nil
	}
	repos, err := m.db.GetRepos()
	if err != nil {
		return fmt.Errorf("list registered repositories: %w", err)
	}
	for _, repo := range repos {
		refs, err := m.startupManagedGateRefs(repo)
		if err != nil {
			return fmt.Errorf("enumerate managed refs for repository %s: %w", repo.ID, err)
		}
		for _, ref := range refs {
			if err := m.ensureManagedGateGuard(repo, ref); err != nil {
				quarantine, quarantineErr := m.db.GetGateRefQuarantine(repo.ID, m.paths.RepoDir(repo.ID), ref)
				if quarantineErr != nil {
					return fmt.Errorf("read managed-ref quarantine for repository %s ref %s: %w", repo.ID, ref, quarantineErr)
				}
				if quarantine != nil {
					continue
				}
				managed, managedErr := m.db.GetManagedGateRef(repo.ID, m.paths.RepoDir(repo.ID), ref)
				if managedErr != nil {
					return fmt.Errorf("read managed-ref journal for repository %s ref %s: %w", repo.ID, ref, managedErr)
				}
				expected := ""
				if managed != nil {
					expected = managed.Head
				}
				observed, observedErr := git.Run(git.WithSanitizedGateConfig(context.Background()), m.paths.RepoDir(repo.ID), "rev-parse", ref+"^{commit}")
				if observedErr != nil {
					observed = ""
				}
				quarantineFn := m.quarantineGateRef
				if quarantineFn == nil {
					quarantineFn = m.db.QuarantineGateRef
				}
				if quarantineErr := quarantineFn(repo.ID, m.paths.RepoDir(repo.ID), ref, expected, strings.TrimSpace(observed), "managed-gate-authority-unavailable"); quarantineErr != nil {
					return fmt.Errorf("persist managed-ref quarantine for repository %s ref %s: %w", repo.ID, ref, quarantineErr)
				}
			}
		}
	}
	return nil
}

func (m *RunManager) startupManagedGateRefs(repo *db.Repo) ([]string, error) {
	if repo == nil {
		return nil, fmt.Errorf("startup managed gate refs require a repository")
	}
	gateDir := m.paths.RepoDir(repo.ID)
	refs := make(map[string]struct{})
	managed, err := m.db.ListManagedGateRefs(repo.ID, gateDir)
	if err != nil {
		return nil, err
	}
	for _, item := range managed {
		refs[item.Ref] = struct{}{}
	}
	out, err := git.Run(git.WithSanitizedGateConfig(context.Background()), gateDir, "for-each-ref", "--format=%(refname)", "refs/heads")
	if err != nil {
		return nil, err
	}
	for _, ref := range strings.Fields(out) {
		if strings.HasPrefix(ref, "refs/heads/") {
			refs[ref] = struct{}{}
		}
	}
	result := make([]string, 0, len(refs))
	for ref := range refs {
		result = append(result, ref)
	}
	sort.Strings(result)
	return result, nil
}

func (m *RunManager) HandleRecover(ctx context.Context, repoID, branch string, keepLocal bool, requestedWorkDir string) (branchsync.State, error) {
	repo, err := m.db.GetRepo(strings.TrimSpace(repoID))
	if err != nil {
		return branchsync.State{}, fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return branchsync.State{}, fmt.Errorf("unknown repo %s", repoID)
	}
	branch = strings.TrimSpace(branch)
	if branch == "" || branch == "HEAD" || strings.Contains(branch, "..") || strings.ContainsAny(branch, "\x00\n") {
		return branchsync.State{}, fmt.Errorf("invalid recovery branch")
	}
	ref := "refs/heads/" + branch
	workDir := repo.WorkingPath
	if strings.TrimSpace(requestedWorkDir) != "" {
		resolved, resolveErr := filepath.Abs(requestedWorkDir)
		if resolveErr != nil {
			return branchsync.State{}, fmt.Errorf("resolve recovery worktree: %w", resolveErr)
		}
		mainRoot, rootErr := git.FindMainRepoRoot(resolved)
		if rootErr != nil || !samePath(mainRoot, repo.WorkingPath) {
			return branchsync.State{}, fmt.Errorf("recovery worktree does not belong to registered repository")
		}
		workDir = resolved
	}
	service := &branchsync.Service{
		DB:                        m.db,
		Repo:                      repo,
		WorkDir:                   workDir,
		GateDir:                   m.paths.RepoDir(repo.ID),
		Paths:                     m.paths,
		ManagedGateRefMutation:    m.managedGateRefMutation(repo.ID, m.paths.RepoDir(repo.ID)),
		ManagedGateRefRead:        m.managedGateRefRead(repo.ID, m.paths.RepoDir(repo.ID), ref),
		ManagedGateRefFinalize:    m.managedGateRefFinalize(repo.ID, m.paths.RepoDir(repo.ID), ref),
		ManagedPrivateRefMutation: m.managedPrivateRefMutation,
		LegacyPublicationProof:    m.legacyPublicationProof,
		PublicationLedgerValidate: m.validatePublicationLedger,
	}
	return service.Recover(ctx, keepLocal), nil
}

func (m *RunManager) validatePublicationLedger(ctx context.Context, run *db.Run) error {
	if m == nil || m.db == nil || run == nil {
		return fmt.Errorf("submission-time publication target ledger is unavailable")
	}
	if run.PRURL != nil && strings.TrimSpace(*run.PRURL) != "" || run.LastPushedSHA != nil || run.PublicationAttemptHeadSHA != nil {
		return fmt.Errorf("legacy publication evidence is present")
	}
	if run.SubmittedHeadSHA == nil || !reviewObjectID(*run.SubmittedHeadSHA) {
		return fmt.Errorf("publication ledger has no canonical submitted head")
	}
	if err := m.db.ValidateRunPublicationTargetLedger(run.ID); err != nil {
		return fmt.Errorf("validate publication target ledger: %w", err)
	}
	repo, err := m.db.GetRepo(run.RepoID)
	if err != nil || repo == nil {
		return fmt.Errorf("publication target repository is unavailable")
	}
	inputs, err := m.publicationTargetInputs(ctx, repo, run.Branch, *run.SubmittedHeadSHA)
	if err != nil {
		return fmt.Errorf("enumerate publication targets: %w", err)
	}
	recorded, err := m.db.ListRunPublicationTargets(run.ID)
	if err != nil {
		return fmt.Errorf("read publication target ledger: %w", err)
	}
	byFingerprint := make(map[string]db.PublicationTargetInput, len(inputs))
	for _, input := range inputs {
		byFingerprint[input.TargetFingerprint] = input
	}
	if len(recorded) != len(inputs) {
		return fmt.Errorf("publication target set changed")
	}
	for _, target := range recorded {
		input, ok := byFingerprint[target.TargetFingerprint]
		lineagePending := strings.TrimSpace(target.RequestLineage) == "" || target.RequestLineage == db.PublicationTargetRequestLineageMigrationPending
		if !ok || input.TargetKind != target.TargetKind || input.Ref != target.Ref || input.TargetVersion != target.TargetVersion || !lineagePending && input.RequestLineage != target.RequestLineage {
			return fmt.Errorf("publication target set changed")
		}
	}
	if run.PublicationEvidenceHash != "" && run.PublicationEvidenceGeneration > 0 {
		if err := m.db.ValidateRunPublicationEvidence(run); err != nil {
			return fmt.Errorf("validate durable publication evidence: %w", err)
		}
		return nil
	}
	publicationTargets, err := m.publicationTargetURLs(ctx, repo, run.Branch)
	if err != nil {
		return fmt.Errorf("enumerate publication targets for historical proof: %w", err)
	}
	targetURLs := make([]string, 0, len(publicationTargets))
	for _, target := range publicationTargets {
		targetURLs = append(targetURLs, target.url)
	}
	remoteBefore, err := m.verifyRemotePublicationSnapshotWithRequestRefs(ctx, run, repo, publicationTargets, recorded, publicationRequestRefsFromInputs(inputs))
	if err != nil {
		return fmt.Errorf("verify pre-cutoff remote publication snapshot: %w", err)
	}
	providerEvidence, err := m.legacyPublicationEvidence(ctx, run, run.Branch, targetURLs)
	if err != nil {
		return fmt.Errorf("verify historical publication proof: %w", err)
	}
	cutoffs, err := publicationCutoffsFromEvidence(providerEvidence)
	if err != nil {
		return fmt.Errorf("verify provider publication cutoff: %w", err)
	}
	proofCtx := withPublicationCutoffs(ctx, cutoffs)
	recorded, err = m.db.ListRunPublicationTargets(run.ID)
	if err != nil {
		return fmt.Errorf("reload reconciled publication target ledger: %w", err)
	}
	requestRefs := publicationRequestRefs(providerEvidence)
	remoteEvidence, err := m.verifyRemotePublicationSnapshotWithRequestRefs(proofCtx, run, repo, publicationTargets, recorded, requestRefs)
	if err != nil {
		return fmt.Errorf("verify remote publication snapshot: %w", err)
	}
	providerEvidenceAgain, err := m.legacyPublicationEvidence(proofCtx, run, run.Branch, targetURLs)
	if err != nil {
		return fmt.Errorf("verify historical publication stability: %w", err)
	}
	if !equalPublicationEvidence(providerEvidence, providerEvidenceAgain) {
		return fmt.Errorf("publication history evidence did not stabilize")
	}
	remoteEvidenceAgain, err := m.verifyRemotePublicationSnapshotWithRequestRefs(proofCtx, run, repo, publicationTargets, recorded, publicationRequestRefs(providerEvidenceAgain))
	if err != nil {
		return fmt.Errorf("verify final remote publication snapshot: %w", err)
	}
	if !equalRemotePublicationEvidence(remoteBefore, remoteEvidence) || !equalRemotePublicationEvidence(remoteEvidence, remoteEvidenceAgain) {
		return fmt.Errorf("remote publication evidence did not stabilize")
	}
	lineageUpdates, err := publicationLineageUpdates(recorded, providerEvidence)
	if err != nil {
		return fmt.Errorf("reconcile publication request lineage: %w", err)
	}
	evidence := make([]db.PublicationEvidenceInput, 0, len(recorded))
	for _, target := range recorded {
		remote, ok := remoteBefore[target.TargetFingerprint]
		if !ok {
			return fmt.Errorf("publication evidence is missing remote target %s", target.TargetFingerprint)
		}
		provider, ok := providerEvidence[target.TargetFingerprint]
		if !ok {
			return fmt.Errorf("publication evidence is missing provider target %s", target.TargetFingerprint)
		}
		evidence = append(evidence, db.PublicationEvidenceInput{
			TargetFingerprint: target.TargetFingerprint,
			Ref:               target.Ref,
			TargetVersion:     target.TargetVersion,
			RemoteHash:        remote.Hash,
			ProviderHash:      provider.Hash,
			Cursor:            provider.Cursor + "|" + remote.Cursor,
			Since:             run.CreatedAt,
			Until:             run.UpdatedAt,
		})
	}
	set, err := m.db.RecordRunPublicationEvidenceWithLineage(run.ID, lineageUpdates, evidence)
	if err != nil {
		return fmt.Errorf("record publication evidence: %w", err)
	}
	run.PublicationEvidenceHash = set.EvidenceHash
	run.PublicationEvidenceGeneration = set.EvidenceGeneration
	return nil
}

func (m *RunManager) legacyPublicationProof(ctx context.Context, run *db.Run, branch string, targets []string) error {
	_, err := m.legacyPublicationEvidence(ctx, run, branch, targets)
	return err
}

func (m *RunManager) legacyPublicationEvidence(ctx context.Context, run *db.Run, branch string, targets []string) (map[string]scm.HistoricalPublicationEvidence, error) {
	if run == nil || !reviewObjectID(run.HeadSHA) || run.SubmittedHeadSHA == nil || !reviewObjectID(*run.SubmittedHeadSHA) {
		return nil, fmt.Errorf("legacy publication proof has no canonical preserved head")
	}
	submitted := *run.SubmittedHeadSHA
	recorded, err := m.db.ListRunPublicationTargets(run.ID)
	if err != nil {
		return nil, fmt.Errorf("read submission-time publication targets: %w", err)
	}
	recordedByFingerprint := make(map[string]db.RunPublicationTarget, len(recorded))
	for _, item := range recorded {
		recordedByFingerprint[item.TargetFingerprint] = item
	}
	seen := make(map[string]struct{}, len(targets))
	evidence := make(map[string]scm.HistoricalPublicationEvidence, len(targets))
	for _, target := range targets {
		fingerprint := db.PublicationTargetFingerprint(target)
		if _, ok := seen[fingerprint]; ok {
			continue
		}
		seen[fingerprint] = struct{}{}
		cmdFactory := func(cmdCtx context.Context, name string, args ...string) *exec.Cmd {
			cmd := exec.CommandContext(cmdCtx, name, args...)
			shellenv.ConfigureShellCommand(cmd)
			return cmd
		}
		var verifier scm.HistoricalPublicationVerifier
		provider := scm.DetectProviderContext(ctx, target)
		switch provider {
		case scm.ProviderGitHub:
			host := scm.ResolveHost(ctx, target)
			verifier = github.New(cmdFactory, func() bool { _, err := exec.LookPath("gh"); return err == nil }, host, github.HostPrefixedSlugForHost(target, host))
		case scm.ProviderGitLab:
			host := scm.ResolveHost(ctx, target)
			verifier = gitlab.New(cmdFactory, func() bool { _, err := exec.LookPath("glab"); return err == nil }, host, gitlab.ProjectPath(target))
		}
		if run.CreatedAt <= 0 || run.UpdatedAt < run.CreatedAt {
			return nil, fmt.Errorf("historical publication proof has no valid run interval")
		}
		targetIdentity, targetRef, identityErr := m.recoveryTargetIdentityForRun(ctx, target, targets, run)
		if identityErr != nil {
			return nil, identityErr
		}
		targetRecord, ok := recordedByFingerprint[fingerprint]
		if !ok {
			return nil, fmt.Errorf("submission-time publication request lineage is unavailable")
		}
		if provider == scm.ProviderUnknown {
			if !isLocalPublicationTarget(target) {
				return nil, fmt.Errorf("historical publication proof is unavailable for target %s: unsupported provider %s", safeurl.Redact(target), provider)
			}
			if targetIdentity != "" {
				return nil, fmt.Errorf("local publication target has unsupported request identity")
			}
			repo, repoErr := m.db.GetRepo(run.RepoID)
			if repoErr != nil || repo == nil {
				return nil, fmt.Errorf("local publication proof has no registered repository")
			}
			proof, proofErr := m.localPublicationEvidence(ctx, repo.WorkingPath, target, targetRef, submitted, publicationLineageRefs(targetRecord.RequestLineage))
			if proofErr != nil {
				return nil, proofErr
			}
			actualRefs := append([]string(nil), proof.RequestRefs...)
			sort.Strings(actualRefs)
			expectedRefs := publicationLineageRefs(targetRecord.RequestLineage)
			sort.Strings(expectedRefs)
			if !equalStrings(expectedRefs, actualRefs) {
				return nil, fmt.Errorf("submission-time publication request lineage changed")
			}
			evidence[fingerprint] = proof
			continue
		}
		if verifier == nil {
			return nil, fmt.Errorf("historical publication proof is unavailable for %s", provider)
		}
		targetVerifier, ok := verifier.(scm.HistoricalTargetPublicationVerifier)
		if !ok {
			return nil, fmt.Errorf("historical target publication proof is unavailable for %s", provider)
		}
		targetBranch := strings.TrimPrefix(targetRef, "refs/heads/")
		if targetIdentity != "" {
			if err := verifier.VerifyUnpublishedHistory(ctx, targetBranch, submitted, run.HeadSHA, run.CreatedAt, run.UpdatedAt, targetIdentity); err != nil {
				return nil, err
			}
		}
		var proof scm.HistoricalPublicationEvidence
		var err error
		if verifierAtCutoff, ok := verifier.(scm.HistoricalTargetPublicationVerifierAtCutoff); ok {
			proof, err = verifierAtCutoff.VerifyUnpublishedTargetHistoryAtCutoff(ctx, targetBranch, submitted, run.HeadSHA, run.CreatedAt, run.UpdatedAt, publicationCutoff(ctx, fingerprint))
		} else {
			if publicationCutoff(ctx, fingerprint) != 0 {
				return nil, fmt.Errorf("historical publication verifier cannot honor the provider cutoff")
			}
			proof, err = targetVerifier.VerifyUnpublishedTargetHistory(ctx, targetBranch, submitted, run.HeadSHA, run.CreatedAt, run.UpdatedAt)
		}
		if err != nil {
			return nil, err
		}
		actualRefs := append([]string(nil), proof.RequestRefs...)
		sort.Strings(actualRefs)
		if strings.TrimSpace(targetRecord.RequestLineage) != "" && targetRecord.RequestLineage != db.PublicationTargetRequestLineageMigrationPending {
			expectedRefs := publicationLineageRefs(targetRecord.RequestLineage)
			sort.Strings(expectedRefs)
			if !equalStrings(expectedRefs, actualRefs) {
				return nil, fmt.Errorf("submission-time publication request lineage changed")
			}
		}
		if !proof.Complete || strings.TrimSpace(proof.HighWater) == "" || !strings.Contains(proof.Coverage, "audit") || !strings.Contains(proof.Coverage, "hasNextPage=false") {
			return nil, fmt.Errorf("historical publication proof for %s is incomplete", provider)
		}
		evidence[fingerprint] = proof
	}
	return evidence, nil
}

func isLocalPublicationTarget(target string) bool {
	target = strings.TrimSpace(target)
	if target == "." || target == ".." || strings.HasPrefix(target, "./") || strings.HasPrefix(target, "../") {
		return true
	}
	return filepath.IsAbs(target) || strings.HasPrefix(strings.ToLower(target), "file://")
}

func (m *RunManager) localPublicationEvidence(ctx context.Context, workDir, target, ref, submitted string, requestRefs []string) (scm.HistoricalPublicationEvidence, error) {
	current, err := git.LsRemote(ctx, workDir, target, ref)
	if err != nil {
		return scm.HistoricalPublicationEvidence{}, fmt.Errorf("read local publication branch %s: %s", ref, safeurl.RedactText(err.Error()))
	}
	if current != submitted {
		return scm.HistoricalPublicationEvidence{}, fmt.Errorf("local publication branch %s is %s, want submitted head %s", ref, current, submitted)
	}
	for _, requestRef := range requestRefs {
		requestHead, requestErr := git.LsRemote(ctx, workDir, target, requestRef)
		if requestErr != nil {
			return scm.HistoricalPublicationEvidence{}, fmt.Errorf("read local publication ref %s: %s", requestRef, safeurl.RedactText(requestErr.Error()))
		}
		if requestHead != submitted {
			return scm.HistoricalPublicationEvidence{}, fmt.Errorf("local publication ref %s is not the submitted head", requestRef)
		}
	}
	if !localRemotePublicationRefsClean(ctx, workDir, target, submitted) {
		return scm.HistoricalPublicationEvidence{}, fmt.Errorf("local publication refs are not an exact submitted-head snapshot")
	}
	parts := append([]string{target, ref, current}, requestRefs...)
	hash := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return scm.HistoricalPublicationEvidence{
		Hash:        hex.EncodeToString(hash[:]),
		Cursor:      "local-exact-refs",
		Coverage:    "exact-refs;local-target",
		HighWater:   "local-snapshot",
		Complete:    true,
		RequestRefs: append([]string(nil), requestRefs...),
	}, nil
}

func localRemotePublicationRefsClean(ctx context.Context, dir, remote, submitted string) bool {
	for _, pattern := range []string{"refs/pull/*/head", "refs/merge-requests/*/head", "refs/changes/*"} {
		out, err := git.Run(ctx, dir, "ls-remote", remote, pattern)
		if err != nil {
			return false
		}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) != 2 || !reviewObjectID(fields[0]) || strings.TrimSpace(fields[1]) == "" || fields[0] != submitted {
				return false
			}
		}
	}
	return true
}

func publicationLineageRefs(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" || value == "none" {
		return nil
	}
	parts := strings.Split(value, ",")
	refs := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			refs = append(refs, part)
		}
	}
	return refs
}

func (m *RunManager) verifyRemotePublicationProof(ctx context.Context, run *db.Run, repo *db.Repo, targets []publicationTargetURL, recorded []db.RunPublicationTarget) error {
	_, err := m.verifyRemotePublicationSnapshot(ctx, run, repo, targets, recorded)
	return err
}

func (m *RunManager) verifyRemotePublicationSnapshot(ctx context.Context, run *db.Run, repo *db.Repo, targets []publicationTargetURL, recorded []db.RunPublicationTarget) (map[string]scm.HistoricalPublicationEvidence, error) {
	return m.verifyRemotePublicationSnapshotWithRequestRefs(ctx, run, repo, targets, recorded, nil)
}

func (m *RunManager) verifyRemotePublicationSnapshotWithRequestRefs(ctx context.Context, run *db.Run, repo *db.Repo, targets []publicationTargetURL, recorded []db.RunPublicationTarget, requestRefs map[string][]string) (map[string]scm.HistoricalPublicationEvidence, error) {
	if run == nil || repo == nil || run.SubmittedHeadSHA == nil || !reviewObjectID(*run.SubmittedHeadSHA) {
		return nil, fmt.Errorf("remote publication proof has no canonical submitted head")
	}
	if len(targets) != len(recorded) {
		return nil, fmt.Errorf("remote publication target set changed")
	}
	byFingerprint := make(map[string]db.RunPublicationTarget, len(recorded))
	for _, target := range recorded {
		if _, duplicate := byFingerprint[target.TargetFingerprint]; duplicate {
			return nil, fmt.Errorf("remote publication target ledger is duplicated")
		}
		byFingerprint[target.TargetFingerprint] = target
	}
	return m.readRemotePublicationSnapshot(ctx, run, repo, targets, recorded, byFingerprint, requestRefs)
}

func (m *RunManager) readRemotePublicationSnapshot(ctx context.Context, run *db.Run, repo *db.Repo, targets []publicationTargetURL, recorded []db.RunPublicationTarget, byFingerprint map[string]db.RunPublicationTarget, requestRefs map[string][]string) (map[string]scm.HistoricalPublicationEvidence, error) {
	seen := make(map[string]struct{}, len(targets))
	evidence := make(map[string]scm.HistoricalPublicationEvidence, len(targets))
	for _, target := range targets {
		fingerprint := db.PublicationTargetFingerprint(target.url)
		record, ok := byFingerprint[fingerprint]
		if !ok || record.Ref == "" || record.State != db.PublicationTargetNoAttempt {
			return nil, fmt.Errorf("remote publication target is not bound to the durable ledger")
		}
		current, err := git.LsRemote(ctx, repo.WorkingPath, target.url, record.Ref)
		if err != nil {
			return nil, fmt.Errorf("read remote branch %s from %s: %s", record.Ref, safeurl.Redact(target.url), safeurl.RedactText(err.Error()))
		}
		if current != *run.SubmittedHeadSHA {
			return nil, fmt.Errorf("remote branch %s from %s is %s, want submitted head %s", record.Ref, safeurl.Redact(target.url), current, *run.SubmittedHeadSHA)
		}
		parts := []string{fingerprint, record.Ref, current}
		checkedRefs := make(map[string]struct{})
		checkRequestRef := func(requestRef string) error {
			requestRef = strings.TrimSpace(requestRef)
			if requestRef == "" {
				return nil
			}
			if _, ok := checkedRefs[requestRef]; ok {
				return nil
			}
			checkedRefs[requestRef] = struct{}{}
			requestHead, err := git.LsRemote(ctx, repo.WorkingPath, target.url, requestRef)
			if err != nil {
				return fmt.Errorf("read exact publication ref %s from %s: %s", requestRef, safeurl.Redact(target.url), safeurl.RedactText(err.Error()))
			}
			if requestHead != *run.SubmittedHeadSHA {
				return fmt.Errorf("exact publication ref %s from %s is not the submitted head", requestRef, safeurl.Redact(target.url))
			}
			parts = append(parts, requestRef, requestHead)
			return nil
		}
		for _, requestRef := range requestRefs[fingerprint] {
			if err := checkRequestRef(requestRef); err != nil {
				return nil, err
			}
		}
		hash := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
		cutoff := publicationCutoff(ctx, fingerprint)
		cursor := "remote-cutoff=unbound;" + hex.EncodeToString(hash[:8])
		highWater := "pre-cutoff"
		coverage := "exact-refs;pre-cutoff"
		if cutoff > 0 {
			cursor = fmt.Sprintf("remote-cutoff=%d;%s", cutoff, hex.EncodeToString(hash[:8]))
			highWater = fmt.Sprintf("provider-date:%d", cutoff)
			coverage = fmt.Sprintf("exact-refs;history-bound;provider-date=%d", cutoff)
		}
		evidence[fingerprint] = scm.HistoricalPublicationEvidence{Hash: hex.EncodeToString(hash[:]), Cursor: cursor, Coverage: coverage, HighWater: highWater, Complete: true}
		seen[fingerprint] = struct{}{}
	}
	if len(seen) != len(recorded) {
		return nil, fmt.Errorf("remote publication target set changed")
	}
	return evidence, nil
}

func equalPublicationEvidence(left, right map[string]scm.HistoricalPublicationEvidence) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		other, ok := right[key]
		if !ok || value.Hash != other.Hash || value.Cursor != other.Cursor || value.Coverage != other.Coverage || value.HighWater != other.HighWater || value.Complete != other.Complete || !equalStrings(value.RequestRefs, other.RequestRefs) {
			return false
		}
	}
	return true
}

func equalRemotePublicationEvidence(left, right map[string]scm.HistoricalPublicationEvidence) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		other, ok := right[key]
		if !ok || value.Hash != other.Hash || !value.Complete || !other.Complete {
			return false
		}
	}
	return true
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func publicationRequestRefs(evidence map[string]scm.HistoricalPublicationEvidence) map[string][]string {
	refs := make(map[string][]string, len(evidence))
	for fingerprint, proof := range evidence {
		refs[fingerprint] = append([]string(nil), proof.RequestRefs...)
	}
	return refs
}

func publicationRequestRefsFromTargets(targets []db.RunPublicationTarget) map[string][]string {
	refs := make(map[string][]string, len(targets))
	for _, target := range targets {
		if target.RequestLineage == db.PublicationTargetRequestLineageMigrationPending {
			continue
		}
		refs[target.TargetFingerprint] = publicationLineageRefs(target.RequestLineage)
	}
	return refs
}

func publicationRequestRefsFromInputs(inputs []db.PublicationTargetInput) map[string][]string {
	refs := make(map[string][]string, len(inputs))
	for _, input := range inputs {
		refs[input.TargetFingerprint] = publicationLineageRefs(input.RequestLineage)
	}
	return refs
}

func publicationLineageUpdates(targets []db.RunPublicationTarget, evidence map[string]scm.HistoricalPublicationEvidence) ([]db.PublicationTargetLineageInput, error) {
	updates := make([]db.PublicationTargetLineageInput, 0)
	for _, target := range targets {
		if strings.TrimSpace(target.RequestLineage) != "" && target.RequestLineage != db.PublicationTargetRequestLineageMigrationPending {
			continue
		}
		proof, ok := evidence[target.TargetFingerprint]
		if !ok {
			return nil, fmt.Errorf("publication evidence is missing target %s", target.TargetFingerprint)
		}
		refs := append([]string(nil), proof.RequestRefs...)
		sort.Strings(refs)
		lineage := "none"
		if len(refs) > 0 {
			lineage = strings.Join(refs, ",")
		}
		updates = append(updates, db.PublicationTargetLineageInput{TargetFingerprint: target.TargetFingerprint, RequestLineage: lineage})
	}
	sort.Slice(updates, func(i, j int) bool { return updates[i].TargetFingerprint < updates[j].TargetFingerprint })
	return updates, nil
}

func publicationRequestRef(ctx context.Context, target, identity string) (string, error) {
	number, err := scm.ExtractPRNumber(identity)
	if err != nil {
		return "", fmt.Errorf("submission-time publication request identity is invalid")
	}
	switch scm.DetectProviderContext(ctx, target) {
	case scm.ProviderGitHub:
		return "refs/pull/" + number + "/head", nil
	case scm.ProviderGitLab:
		return "refs/merge-requests/" + number + "/head", nil
	default:
		return "", fmt.Errorf("exact publication ref is unavailable for provider %s", scm.DetectProviderContext(ctx, target))
	}
}

func (m *RunManager) recoveryTargetIdentityForRun(ctx context.Context, target string, targets []string, run *db.Run) (string, string, error) {
	if m == nil || m.db == nil || run == nil {
		return "", "", fmt.Errorf("submission-time publication identity has no durable run ledger")
	}
	recorded, err := m.db.ListRunPublicationTargets(run.ID)
	if err != nil {
		return "", "", fmt.Errorf("read submission-time publication targets: %w", err)
	}
	if err := m.db.ValidateRunPublicationTargetLedger(run.ID); err != nil {
		return "", "", fmt.Errorf("validate submission-time publication target ledger: %w", err)
	}
	targetSet, err := m.db.GetRunPublicationTargetSet(run.ID)
	if err != nil {
		return "", "", fmt.Errorf("read submission-time publication target set: %w", err)
	}
	if targetSet == nil || targetSet.State != db.PublicationTargetSetComplete || targetSet.TargetCount != len(recorded) || targetSet.TargetCount == 0 || targetSet.Generation < 0 || strings.TrimSpace(targetSet.Provenance) == "" || targetSet.TargetSetHash != db.PublicationTargetSetHash(recorded) {
		return "", "", fmt.Errorf("submission-time publication targets are not initialized")
	}
	repo, err := m.db.GetRepo(run.RepoID)
	if err != nil || repo == nil {
		return "", "", fmt.Errorf("submission-time publication target repository is unavailable")
	}
	current := make(map[string]struct{}, len(targets))
	for _, candidate := range targets {
		fingerprint := db.PublicationTargetFingerprint(candidate)
		if fingerprint == "" {
			return "", "", fmt.Errorf("submission-time publication target has no canonical identity")
		}
		current[fingerprint] = struct{}{}
	}
	byFingerprint := make(map[string]*db.RunPublicationTarget, len(recorded))
	for index := range recorded {
		item := &recorded[index]
		if item.TargetKind == "" || !validPublicationTargetFingerprint(item.TargetFingerprint) || item.Ref == "" || item.TargetVersion < 0 || item.TargetVersion != repo.URLVersion || item.Generation < 0 || item.Provenance == "" {
			return "", "", fmt.Errorf("submission-time publication target record is malformed")
		}
		switch item.State {
		case db.PublicationTargetNoAttempt:
			if item.RequestIdentity != "" || item.AttemptHeadSHA != "" {
				return "", "", fmt.Errorf("submission-time publication target no-attempt record is inconsistent")
			}
		default:
			return "", "", fmt.Errorf("submission-time publication target has publication evidence")
		}
		if _, ok := current[item.TargetFingerprint]; !ok {
			return "", "", fmt.Errorf("submission-time publication target is absent from the current target set")
		}
		if _, duplicate := byFingerprint[item.TargetFingerprint]; duplicate {
			return "", "", fmt.Errorf("submission-time publication target identity is duplicated")
		}
		byFingerprint[item.TargetFingerprint] = item
	}
	if targetSet.TargetCount != len(current) {
		return "", "", fmt.Errorf("submission-time publication target set changed")
	}
	for fingerprint := range current {
		if _, ok := byFingerprint[fingerprint]; !ok {
			return "", "", fmt.Errorf("submission-time publication target is missing from the durable ledger")
		}
	}
	targetFingerprint := db.PublicationTargetFingerprint(target)
	targetRecord := byFingerprint[targetFingerprint]
	if targetRecord == nil || targetRecord.Ref != "refs/heads/"+strings.TrimPrefix(strings.TrimSpace(run.Branch), "refs/heads/") {
		return "", "", fmt.Errorf("submission-time publication target ref is not bound to the run branch")
	}
	if targetRecord.PRRequestIdentity != "" {
		identity, err := recoveryTargetIdentity(ctx, target, &targetRecord.PRRequestIdentity)
		if err != nil {
			return "", "", err
		}
		return identity, targetRecord.Ref, nil
	}
	if run.PRURL != nil && strings.TrimSpace(*run.PRURL) != "" {
		legacyPRURL := strings.TrimSpace(*run.PRURL)
		matched := false
		for _, item := range byFingerprint {
			if item.PRRequestIdentity == legacyPRURL {
				matched = true
				break
			}
		}
		if !matched {
			return "", "", fmt.Errorf("run PR identity is not bound to a durable publication target")
		}
	}
	return "", targetRecord.Ref, nil
}

func validPublicationTargetFingerprint(value string) bool {
	if len(value) != hex.EncodedLen(sha256.Size) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func recoveryTargetIdentity(ctx context.Context, target string, prURL *string) (string, error) {
	if prURL == nil || strings.TrimSpace(*prURL) == "" {
		return "", nil
	}
	provider := scm.DetectProviderContext(ctx, target)
	if provider != scm.DetectProviderContext(ctx, *prURL) {
		return "", fmt.Errorf("submission-time publication identity does not match target provider")
	}
	targetHost := scm.ResolveHost(ctx, target)
	prHost := scm.ResolveHost(ctx, *prURL)
	if !strings.EqualFold(targetHost, prHost) {
		return "", fmt.Errorf("submission-time publication identity does not match target host")
	}
	switch provider {
	case scm.ProviderGitHub:
		if github.HostPrefixedSlugForHost(target, targetHost) == github.HostPrefixedSlugForHost(*prURL, prHost) {
			return strings.TrimSpace(*prURL), nil
		}
	case scm.ProviderGitLab:
		if gitlab.ProjectPath(target) == gitlab.ProjectPath(*prURL) {
			return strings.TrimSpace(*prURL), nil
		}
	}
	return "", fmt.Errorf("submission-time publication identity does not match target repository")
}

func reviewObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

type recoveredRunPlan struct {
	run     *db.Run
	repo    *db.Repo
	workDir string
	gateDir string
	cfg     *config.Config
	agent   agent.Agent
	steps   []pipeline.Step
}

func (m *RunManager) recoverableParkedRuns(ctx context.Context) []recoveredRunPlan {
	runs, err := m.db.GetActiveRuns()
	if err != nil {
		slog.Error("failed to list active runs for recovery", "error", err)
		return nil
	}
	plans := make([]recoveredRunPlan, 0, len(runs))
	branchCounts := make(map[string]int, len(runs))
	for _, run := range runs {
		branchCounts[run.RepoID+"\x00"+run.Branch]++
	}
	for _, run := range runs {
		if branchCounts[run.RepoID+"\x00"+run.Branch] != 1 {
			slog.Warn("active run cannot be safely resumed", "run_id", run.ID, "error", "conflicting active run for branch")
			continue
		}
		plan, err := m.prepareRecoveredRun(ctx, run)
		if err != nil {
			slog.Warn("active run cannot be safely resumed", "run_id", run.ID, "error", err)
			continue
		}
		plans = append(plans, *plan)
	}
	return plans
}

func (m *RunManager) prepareRecoveredRun(ctx context.Context, run *db.Run) (*recoveredRunPlan, error) {
	if run == nil || run.Status != types.RunRunning || run.AwaitingAgentSince == nil || run.Branch == "" {
		return nil, fmt.Errorf("run is not a parked running run")
	}
	repo, err := m.db.GetRepo(run.RepoID)
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return nil, fmt.Errorf("run repository is missing")
	}
	workDir := m.paths.WorktreeDir(repo.ID, run.ID)
	if info, err := os.Stat(workDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("worktree is missing")
	}
	headSHA, err := git.HeadSHA(ctx, workDir)
	if err != nil || headSHA != run.HeadSHA {
		return nil, fmt.Errorf("worktree head does not match run head")
	}
	gateDir := m.paths.RepoDir(repo.ID)
	commonDir, err := git.Run(ctx, workDir, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("resolve worktree common git dir: %w", err)
	}
	if !samePath(resolveGitPath(workDir, commonDir), gateDir) {
		return nil, fmt.Errorf("worktree does not belong to its gate repository")
	}

	execSteps := m.steps()
	if err := pipeline.ValidateRecoveredRun(m.db, run, execSteps); err != nil {
		return nil, err
	}
	cfg, err := m.loadRecoveredConfig(ctx, run, repo, workDir)
	if err != nil {
		return nil, err
	}
	ag, err := newPipelineAgent(ctx, cfg, exec.LookPath)
	if err != nil {
		return nil, err
	}
	if cfg.SessionReuse {
		if err := validateRecoveredSessionProviders(m.db, run.ID, ag); err != nil {
			_ = ag.Close()
			return nil, err
		}
	}
	return &recoveredRunPlan{
		run:     run,
		repo:    repo,
		workDir: workDir,
		gateDir: gateDir,
		cfg:     cfg,
		agent:   ag,
		steps:   execSteps,
	}, nil
}

func validateRecoveredSessionProviders(database *db.DB, runID string, ag agent.Agent) error {
	sessions, err := database.GetRunAgentSessions(runID)
	if err != nil {
		return fmt.Errorf("get run sessions: %w", err)
	}
	for _, session := range sessions {
		if session.Role != string(pipeline.SessionRoleReviewer) && session.Role != string(pipeline.SessionRoleFixer) {
			return fmt.Errorf("recovered run has unknown session role %q", session.Role)
		}
		if session.Agent == "" || session.SessionID == "" {
			return fmt.Errorf("recovered run has incomplete session metadata")
		}
		if session.Role == string(pipeline.SessionRoleFixer) && !agent.SupportsSessionProvider(ag, session.Agent) {
			return fmt.Errorf("session provider %q is no longer configured", session.Agent)
		}
	}
	return nil
}

func (m *RunManager) loadRecoveredConfig(ctx context.Context, run *db.Run, repo *db.Repo, workDir string) (*config.Config, error) {
	globalCfg, err := config.LoadGlobal(m.paths.ConfigFile())
	if err != nil {
		return nil, fmt.Errorf("load global config: %w", err)
	}
	repoCfg, err := config.LoadRepo(workDir)
	if err != nil {
		return nil, fmt.Errorf("load repo config: %w", err)
	}
	var trustedSHA string
	if repo.DefaultBranch != "" {
		fetchCtx, cancel := context.WithTimeout(ctx, recoveredConfigFetchTimeout)
		defer cancel()
		if err := fetchRecoveredRemoteBranch(fetchCtx, workDir, "origin", repo.DefaultBranch); err != nil {
			slog.Warn("failed to fetch default branch while recovering run; trusted config disabled", "run_id", run.ID, "branch", repo.DefaultBranch, "error", err)
		} else if sha, err := git.ResolveRef(ctx, workDir, "refs/remotes/origin/"+repo.DefaultBranch); err != nil {
			slog.Warn("failed to resolve default branch while recovering run; trusted config disabled", "run_id", run.ID, "branch", repo.DefaultBranch, "error", err)
		} else {
			trustedSHA = sha
		}
	}
	// SECURITY: a trusted-config fetch failure must abort, not silently disable
	// the disable_project_settings opt-out (see assertGateTrustedConfigReadable).
	if err := assertGateTrustedConfigReadable(ctx, workDir, repo.DefaultBranch, trustedSHA); err != nil {
		return nil, err
	}
	trustedRepoCfg := loadTrustedRepoConfig(ctx, workDir, trustedSHA, run.ID)
	allowRepoCommands := trustedRepoCfg != nil && trustedRepoCfg.AllowRepoCommands
	return config.Merge(globalCfg, config.EffectiveRepoConfig(repoCfg, trustedRepoCfg, allowRepoCommands)), nil
}

func newPipelineAgent(ctx context.Context, cfg *config.Config, lookPath func(string) (string, error)) (agent.Agent, error) {
	if steps.IsDemoMode() {
		return agent.NewNoop(), nil
	}
	if err := cfg.ResolveAgent(ctx, lookPath); err != nil {
		return nil, err
	}
	agents := cfg.Agents
	if len(agents) == 0 {
		agents = []types.AgentName{cfg.Agent}
	}
	created := make([]agent.Agent, 0, len(agents))
	for _, name := range agents {
		next, err := agent.NewWithOptions(name, cfg.AgentPathFor(name), cfg.AgentArgsFor(name), agent.Options{
			ACPRegistryOverrides:   cfg.ACPRegistryOverrides,
			DisableProjectSettings: cfg.DisableProjectSettings,
		})
		if err != nil {
			for _, existing := range created {
				_ = existing.Close()
			}
			return nil, fmt.Errorf("create agent %s: %w", name, err)
		}
		created = append(created, agent.WithSteering(next))
	}
	ag := agent.NewFallback(created)
	// Fail closed ONLY under the trusted opt-out (see startRun): refuse an
	// unverified harness when the repo disabled project settings; otherwise run
	// every adapter as before.
	if cfg.DisableProjectSettings {
		if err := agent.EnsureGateNeutralized(ag); err != nil {
			_ = ag.Close()
			return nil, err
		}
	}
	return ag, nil
}

func resolveGitPath(workDir, value string) string {
	value = strings.TrimSpace(value)
	if !filepath.IsAbs(value) {
		value = filepath.Join(workDir, value)
	}
	return filepath.Clean(value)
}

func samePath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if resolved, err := filepath.EvalSymlinks(a); err == nil {
		a = resolved
	}
	if resolved, err := filepath.EvalSymlinks(b); err == nil {
		b = resolved
	}
	return a == b
}

func (m *RunManager) resumeRecoveredRuns(plans []recoveredRunPlan) {
	for _, plan := range plans {
		m.resumeRecoveredRun(plan)
	}
}

func (m *RunManager) resumeRecoveredRun(plan recoveredRunPlan) {
	if m.shuttingDown.Load() {
		_ = plan.agent.Close()
		return
	}
	ownershipLock, err := branchsync.AcquireBranchOwnershipLock(m.paths, plan.repo, plan.workDir, plan.run.Branch)
	if err != nil {
		_ = plan.agent.Close()
		slog.Warn("active run cannot be resumed", "run_id", plan.run.ID, "error", err)
		return
	}
	defer ownershipLock.Release()
	runCtx, cancel := context.WithCancelCause(context.Background())
	executor := pipeline.NewExecutor(m.db, m.paths, plan.cfg, plan.agent, plan.steps, m.broadcast)
	executor.SetBranchRefUpdater(m.pipelineBranchRefUpdater(plan.repo))
	done := make(chan struct{})
	m.mu.Lock()
	m.executors[plan.run.ID] = executor
	m.cancels[plan.run.ID] = cancel
	m.dones[plan.run.ID] = done
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		startedAt := time.Now()
		defer m.wg.Done()
		defer close(done)
		defer func() {
			if recovered := recover(); recovered != nil {
				errMsg := fmt.Sprintf("internal panic: %v", recovered)
				plan.run.Status = types.RunFailed
				plan.run.Error = &errMsg
				if err := m.db.UpdateRunErrorStatus(plan.run.ID, errMsg, types.RunFailed); err != nil {
					slog.Error("failed to update recovered run after panic", "run_id", plan.run.ID, "error", err)
				}
			}
			cancel(nil)
			_ = plan.agent.Close()
			m.closeSubscribers(plan.run.ID)
			if err := git.WorktreeRemove(context.Background(), plan.gateDir, plan.workDir); err != nil {
				slog.Warn("failed to remove recovered worktree", "path", plan.workDir, "error", err)
			}
			m.mu.Lock()
			delete(m.executors, plan.run.ID)
			delete(m.cancels, plan.run.ID)
			delete(m.dones, plan.run.ID)
			m.mu.Unlock()
		}()

		if err := executor.Resume(runCtx, plan.run, plan.repo, plan.workDir); err != nil {
			if plan.run.Status == types.RunRunning {
				errMsg := err.Error()
				plan.run.Status = types.RunFailed
				plan.run.Error = &errMsg
				if dbErr := m.db.UpdateRunErrorStatus(plan.run.ID, errMsg, types.RunFailed); dbErr != nil {
					slog.Error("failed to mark recovered run failed", "run_id", plan.run.ID, "error", dbErr)
				}
			}
			slog.Error("recovered pipeline failed", "run_id", plan.run.ID, "error", err)
		}
		fields := telemetry.Fields{
			"action":      "finished",
			"trigger":     "recovery",
			"agent":       string(plan.cfg.Agent),
			"branch_role": telemetryBranchRole(plan.run.Branch, plan.repo.DefaultBranch),
			"status":      string(plan.run.Status),
			"duration_ms": time.Since(startedAt).Milliseconds(),
			"step_count":  len(plan.steps),
			"pr_created":  plan.run.PRURL != nil && *plan.run.PRURL != "",
		}
		if failedStep := telemetryFailedStepName(m.db, plan.run.ID); failedStep != "" {
			fields["failed_step"] = failedStep
		}
		addRunPerformanceSummary(m.db, plan.run.ID, fields)
		telemetry.Track("run", fields)
	}()
}

func agentListsEqual(a, b []types.AgentName) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Subscribe registers a subscriber mailbox for a run.
//
// The returned subscription always opens with a stream-gap frame, so a
// subscriber's first action is always one authoritative read. That makes
// attach and reconnect converge without each consumer needing its own
// subscribe-then-reconcile ordering rule. A run that has already completed
// yields that one gap and then finishes.
func (m *RunManager) Subscribe(runID string) (*Subscription, error) {
	m.subMu.Lock()
	defer m.subMu.Unlock()

	mb := newEventMailbox(runID, m.stateRevs[runID])
	if m.completedRuns[runID] {
		mb.close()
		return &Subscription{mb: mb, unsub: func() {}}, nil
	}
	if len(m.subscribers[runID]) >= maxSubscribersPerRun {
		return nil, fmt.Errorf("run %s already has the maximum of %d event subscribers", runID, maxSubscribersPerRun)
	}
	m.subscribers[runID] = append(m.subscribers[runID], mb)

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			m.subMu.Lock()
			subs := m.subscribers[runID]
			for i, s := range subs {
				if s == mb {
					m.subscribers[runID] = append(subs[:i], subs[i+1:]...)
					break
				}
			}
			if len(m.subscribers[runID]) == 0 {
				delete(m.subscribers, runID)
			}
			m.subMu.Unlock()
			mb.release()
		})
	}
	return &Subscription{mb: mb, unsub: unsub}, nil
}

// Subscription is one subscriber's view of a run's event stream. It owns no
// goroutine: the caller drives it with Next.
type Subscription struct {
	mb    *eventMailbox
	unsub func()
}

// Next blocks until the next frame is available and returns it. ok is false
// once the stream is finished or ctx is done.
func (s *Subscription) Next(ctx context.Context) (ipc.Event, bool) { return s.mb.next(ctx) }

// Close unsubscribes and releases every retained payload. It is idempotent.
func (s *Subscription) Close() { s.unsub() }

// StateRev returns the current monotonic state revision for a run.
//
// A caller serving an authoritative snapshot must sample this BEFORE reading
// the database. Every producer writes state and only then broadcasts, so a
// revision sampled first is never newer than the snapshot that follows it:
// every event at or below it is already reflected in that read, and every
// event above it still reaches the subscriber and still applies on top.
func (m *RunManager) StateRev(runID string) int64 {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	return m.stateRevs[runID]
}

// broadcast stamps a state revision and publishes an event to every subscriber
// of the event's run. It performs no blocking channel operation and no I/O, so
// the executor can never be stalled by a slow or dead subscriber.
func (m *RunManager) broadcast(event ipc.Event) {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	if ipc.ClassOf(event.Type) == ipc.ClassState {
		m.stateRevs[event.RunID]++
		event.StateRev = m.stateRevs[event.RunID]
	}
	for _, mb := range m.subscribers[event.RunID] {
		mb.publish(event)
	}
}

// closeSubscribers soft-closes every subscriber for a run and marks the run
// completed so future Subscribe calls return a gapped, immediately-finished
// subscription. Soft close still drains queued frames and any pending gap, so
// a coalesced terminal transition cannot be discarded by completion.
func (m *RunManager) closeSubscribers(runID string) {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	for _, mb := range m.subscribers[runID] {
		mb.close()
	}
	delete(m.subscribers, runID)
	m.completedRuns[runID] = true
	m.completedOrder = append(m.completedOrder, runID)
	if len(m.completedOrder) > 1000 {
		half := len(m.completedOrder) / 2
		for _, id := range m.completedOrder[:half] {
			delete(m.completedRuns, id)
			delete(m.stateRevs, id)
		}
		m.completedOrder = m.completedOrder[half:]
	}
}

// repoIDFromGatePath extracts the repo ID from a gate bare repo path.
// Gate paths look like: <root>/repos/<id>.git
func repoIDFromGatePath(gatePath string) (string, error) {
	base := filepath.Base(gatePath)
	if !strings.HasSuffix(base, ".git") {
		return "", fmt.Errorf("invalid gate path: %s", gatePath)
	}
	return strings.TrimSuffix(base, ".git"), nil
}

// branchFromRef extracts the branch name from a full git ref.
// "refs/heads/main" → "main", "main" → "main"
func branchFromRef(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
}

// loadTrustedRepoConfig reads .no-mistakes.yaml from the trusted
// default-branch commit (trustedSHA - the exact SHA startRun just fetched and
// resolved) in the worktree and parses it. Reading at a pinned SHA, rather
// than the origin/<defaultBranch> remote-tracking ref, closes the stale-ref
// hole: the gate worktree shares refs with the bare repo, so without a fresh
// fetch + resolve the ref could point at a commit a previous run left behind.
//
// trustedSHA is empty when the default branch is unknown, the fetch failed,
// or the ref did not resolve. The caller must first reject those cases with
// assertGateTrustedConfigReadable; returning nil here remains defensive and
// ensures EffectiveRepoConfig never uses pushed gate-control fields.
func loadTrustedRepoConfig(ctx context.Context, wtDir, trustedSHA, runID string) *config.RepoConfig {
	if trustedSHA == "" {
		// No trusted SHA means no freshly-fetched default-branch commit to
		// read from. Return nil so EffectiveRepoConfig forces empty
		// commands/agent - the secure default - instead of falling back to a
		// potentially stale origin/<defaultBranch> ref.
		return nil
	}
	content, err := git.ShowFile(ctx, wtDir, trustedSHA, ".no-mistakes.yaml")
	if err != nil {
		// Path absent on the default branch is the common "repo has no
		// trusted commands" case; log at debug so it isn't noisy. Other
		// errors are surfaced at warn so a genuinely broken read isn't
		// silent. Either way trusted is nil → fail closed.
		slog.Debug("trusted repo config: not present on default branch", "run_id", runID, "sha", trustedSHA, "error", err)
		return nil
	}
	trusted, err := config.LoadRepoFromBytes([]byte(content))
	if err != nil {
		slog.Warn("trusted repo config: parse failed; commands/agent from pushed branch will be disabled", "run_id", runID, "sha", trustedSHA, "error", err)
		return nil
	}
	return trusted
}

// assertGateTrustedConfigReadable fails a run LOUD when the trusted
// default-branch copy of .no-mistakes.yaml could not be READ at all. This is the
// security correction for disable_project_settings: that field is a boundary
// honored only from the trusted copy, so an unreadable trusted config must NOT
// be silently treated as "not opted out" - no-mistakes cannot know whether the
// repo relies on the boundary, so it refuses to run rather than risk launching a
// gate agent with the project instructions loaded.
//
// It distinguishes "could not read the trusted config at all" (abort) from
// "read the trusted tree fine, there is simply no .no-mistakes.yaml on the
// default branch" (the common ordinary-repo case, which is NOT opted out and
// must proceed). Abort cases:
//   - no known default branch to read a trusted copy from,
//   - the default branch could not be fetched/resolved to a pinned SHA,
//   - the pinned commit or tree is not readable (missing object / partial fetch),
//   - the trusted .no-mistakes.yaml is present but unreadable or unparseable.
func assertGateTrustedConfigReadable(ctx context.Context, wtDir, defaultBranch, trustedSHA string) error {
	if defaultBranch == "" {
		return fmt.Errorf("cannot evaluate disable_project_settings: repository has no known default branch to read trusted config from")
	}
	if trustedSHA == "" {
		return fmt.Errorf("cannot evaluate disable_project_settings: failed to fetch or resolve trusted default branch %q (refusing to run without reading the trusted config)", defaultBranch)
	}
	if _, err := git.Run(ctx, wtDir, "rev-parse", "-q", "--verify", trustedSHA+"^{commit}"); err != nil {
		return fmt.Errorf("cannot evaluate disable_project_settings: trusted default-branch commit %s is not readable: %w", trustedSHA, err)
	}
	entry, err := git.Run(ctx, wtDir, "ls-tree", trustedSHA, "--", ".no-mistakes.yaml")
	if err != nil {
		return fmt.Errorf("cannot evaluate disable_project_settings: trusted default-branch tree at %s is not readable: %w", trustedSHA, err)
	}
	if entry == "" {
		return nil
	}
	content, err := git.ShowFile(ctx, wtDir, trustedSHA, ".no-mistakes.yaml")
	if err != nil {
		return fmt.Errorf("cannot evaluate disable_project_settings: trusted .no-mistakes.yaml at %s is present but not readable: %w", trustedSHA, err)
	}
	if _, err := config.LoadRepoFromBytes([]byte(content)); err != nil {
		return fmt.Errorf("cannot evaluate disable_project_settings: trusted .no-mistakes.yaml at %s is present but unparseable: %w", trustedSHA, err)
	}
	return nil
}

func (m *RunManager) HandleAdmitPush(ctx context.Context, params *ipc.AdmitPushParams) (string, error) {
	if params == nil {
		return "", fmt.Errorf("admit push: missing parameters")
	}
	ids, err := m.HandleAdmitPushBatch(ctx, &ipc.AdmitPushBatchParams{Gate: params.Gate, Updates: []ipc.AdmitPushUpdate{{Ref: params.Ref, Old: params.Old, New: params.New, SkipSteps: params.SkipSteps, Intent: params.Intent}}, ReceiveSessionID: params.ReceiveSessionID, ReceiveCapability: params.ReceiveCapability})
	if err != nil {
		return "", err
	}
	return ids[0], nil
}

func (m *RunManager) HandleAdmitPushBatch(ctx context.Context, params *ipc.AdmitPushBatchParams) ([]string, error) {
	if params == nil || len(params.Updates) == 0 {
		return nil, fmt.Errorf("admit push batch: at least one update is required")
	}
	if strings.TrimSpace(params.ReceiveSessionID) == "" || strings.TrimSpace(params.ReceiveCapability) == "" {
		return nil, fmt.Errorf("admit push batch: authenticated receive capability is required")
	}
	repo, _, err := m.receiveRepo(params.Gate, params.Updates[0].Ref)
	if err != nil {
		return nil, err
	}
	active, err := m.db.VerifyReceiveSession(repo.ID, m.paths.RepoDir(repo.ID), params.ReceiveSessionID, params.ReceiveCapability)
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, fmt.Errorf("admit push batch: receive session capability was not issued for this gate")
	}
	type checkedUpdate struct {
		update ipc.AdmitPushUpdate
		branch string
	}
	checked := make([]checkedUpdate, len(params.Updates))
	locksByBranch := make(map[string]*branchsync.BranchOwnershipLock, len(params.Updates))
	branchKeys := make(map[string]string, len(params.Updates))
	for i, update := range params.Updates {
		otherRepo, branch, err := m.receiveRepo(params.Gate, update.Ref)
		if err != nil {
			return nil, err
		}
		if otherRepo.ID != repo.ID {
			return nil, fmt.Errorf("admit push batch: updates do not target one repository")
		}
		key := update.Ref + "\x00" + update.Old + "\x00" + update.New
		if previous, ok := branchKeys[branch]; ok && previous != key {
			return nil, fmt.Errorf("admit push batch: branch %s has multiple transitions", branch)
		}
		if _, ok := branchKeys[branch]; ok {
			return nil, fmt.Errorf("admit push batch: duplicate transition for %s", update.Ref)
		}
		branchKeys[branch] = key
		checked[i] = checkedUpdate{update: update, branch: branch}
	}
	branches := make([]string, 0, len(branchKeys))
	for branch := range branchKeys {
		branches = append(branches, branch)
	}
	sort.Strings(branches)
	for _, branch := range branches {
		lock, err := branchsync.AcquireBranchOwnershipLock(m.paths, repo, repo.WorkingPath, branch)
		if err != nil {
			for _, held := range locksByBranch {
				held.Release()
			}
			return nil, fmt.Errorf("acquire branch ownership lock: %w", err)
		}
		locksByBranch[branch] = lock
	}
	defer func() {
		for _, branch := range branches {
			locksByBranch[branch].Release()
		}
	}()
	inputs := make([]db.ReceiveReservationInput, len(checked))
	for i, item := range checked {
		current, exists, err := gateReceiveRef(ctx, m.paths.RepoDir(repo.ID), item.update.Ref)
		if err != nil {
			return nil, err
		}
		if err := m.verifyManagedGateRefHead(repo, item.update.Ref, current, exists, item.update.Old, true); err != nil {
			return nil, err
		}
		if !receiveOldMatches(current, exists, item.update.Old) {
			return nil, fmt.Errorf("gate ref %s is not at expected old head %s", item.update.Ref, item.update.Old)
		}
		inputs[i] = db.ReceiveReservationInput{RepoID: repo.ID, GatePath: m.paths.RepoDir(repo.ID), Branch: item.branch, Ref: item.update.Ref, OldSHA: item.update.Old, NewSHA: item.update.New, SkipSteps: item.update.SkipSteps, Intent: item.update.Intent}
	}
	reservations, err := m.db.ReserveReceivesForAuthenticatedSession(params.ReceiveSessionID, params.ReceiveCapability, inputs)
	if err != nil {
		return nil, err
	}
	for _, item := range checked {
		if err := m.releaseManagedGateGuard(repo.ID, item.update.Ref); err != nil {
			return nil, fmt.Errorf("release managed gate authority for receive: %w", err)
		}
	}
	ids := make([]string, len(reservations))
	for i, reservation := range reservations {
		ids[i] = reservation.ID
	}
	return ids, nil
}

func (m *RunManager) HandleReceiveTransaction(ctx context.Context, params *ipc.ReceiveTransactionParams) error {
	if params == nil {
		return fmt.Errorf("receive transaction: missing parameters")
	}
	return m.HandleReceiveTransactionBatch(ctx, &ipc.ReceiveTransactionBatchParams{Gate: params.Gate, Phase: params.Phase, Updates: []ipc.ReceiveTransactionUpdate{{ReservationID: params.ReservationID, Ref: params.Ref, Old: params.Old, New: params.New}}, ReceiveSessionID: params.ReceiveSessionID, ReceiveCapability: params.ReceiveCapability})
}

func (m *RunManager) HandleReceiveTransactionBatch(ctx context.Context, params *ipc.ReceiveTransactionBatchParams) error {
	if params == nil {
		return fmt.Errorf("receive transaction: missing parameters")
	}
	if strings.TrimSpace(params.ReceiveSessionID) == "" || strings.TrimSpace(params.ReceiveCapability) == "" {
		return fmt.Errorf("receive transaction: authenticated receive capability is required")
	}
	if len(params.Updates) == 0 {
		if params.Phase != "aborted" {
			return fmt.Errorf("receive transaction: at least one transition is required")
		}
		repoID, err := repoIDFromGatePath(params.Gate)
		if err != nil {
			return err
		}
		repo, err := m.db.GetRepo(repoID)
		if err != nil {
			return fmt.Errorf("get repo: %w", err)
		}
		if repo == nil || !samePath(params.Gate, m.paths.RepoDir(repo.ID)) {
			return fmt.Errorf("receive transaction: gate path does not match registered repository")
		}
		return m.db.AbortReceiveSession(repo.ID, m.paths.RepoDir(repo.ID), params.ReceiveSessionID, params.ReceiveCapability)
	}
	repo, _, err := m.receiveRepo(params.Gate, params.Updates[0].Ref)
	if err != nil {
		return err
	}
	type checkedUpdate struct {
		update ipc.ReceiveTransactionUpdate
		branch string
	}
	checked := make([]checkedUpdate, len(params.Updates))
	branchesSet := make(map[string]struct{}, len(params.Updates))
	for i, update := range params.Updates {
		otherRepo, branch, err := m.receiveRepo(params.Gate, update.Ref)
		if err != nil {
			return err
		}
		if otherRepo.ID != repo.ID {
			return fmt.Errorf("receive transaction: updates do not target one repository")
		}
		if _, ok := branchesSet[branch]; ok {
			return fmt.Errorf("receive transaction: duplicate branch transition %s", branch)
		}
		branchesSet[branch] = struct{}{}
		checked[i] = checkedUpdate{update: update, branch: branch}
	}
	branches := make([]string, 0, len(branchesSet))
	for branch := range branchesSet {
		branches = append(branches, branch)
	}
	sort.Strings(branches)
	locks := make(map[string]*branchsync.BranchOwnershipLock, len(branches))
	for _, branch := range branches {
		lock, err := branchsync.AcquireBranchOwnershipLock(m.paths, repo, repo.WorkingPath, branch)
		if err != nil {
			for _, held := range locks {
				held.Release()
			}
			return fmt.Errorf("acquire branch ownership lock: %w", err)
		}
		locks[branch] = lock
	}
	defer func() {
		for _, branch := range branches {
			locks[branch].Release()
		}
	}()
	inputs := make([]db.ReceiveTransactionInput, len(checked))
	for i, item := range checked {
		reservation, err := m.db.GetReceiveReservation(item.update.ReservationID)
		if err != nil {
			return err
		}
		if reservation == nil || reservation.RepoID != repo.ID || reservation.Branch != item.branch || reservation.Ref != item.update.Ref || reservation.OldSHA != item.update.Old || reservation.NewSHA != item.update.New || !reservation.MatchesSession(params.ReceiveSessionID, params.ReceiveCapability) {
			return fmt.Errorf("receive transaction: reservation identity does not match the exact receive")
		}
		current, exists, err := gateReceiveRef(ctx, m.paths.RepoDir(repo.ID), item.update.Ref)
		if err != nil {
			return err
		}
		if params.Phase == "committed" {
			if err := m.verifyManagedGateRefCommit(repo, item.update.Ref, current, exists, item.update.Old, item.update.New); err != nil {
				return err
			}
		} else {
			acquireAuthority := params.Phase != "prepared"
			if err := m.verifyManagedGateRefHead(repo, item.update.Ref, current, exists, item.update.Old, acquireAuthority); err != nil {
				return err
			}
			if params.Phase == "prepared" {
				if err := m.releaseManagedGateGuard(repo.ID, item.update.Ref); err != nil {
					return fmt.Errorf("release managed gate authority after receive preparation: %w", err)
				}
			}
		}
		inputs[i] = db.ReceiveTransactionInput{ID: item.update.ReservationID, RepoID: repo.ID, Branch: item.branch, Ref: item.update.Ref, OldSHA: item.update.Old, NewSHA: item.update.New}
	}
	if err := m.db.ApplyReceiveTransactionBatch(params.Phase, params.ReceiveSessionID, params.ReceiveCapability, inputs); err != nil {
		return err
	}
	if params.Phase == "committed" || params.Phase == "aborted" {
		for _, item := range checked {
			if err := m.ensureManagedGateGuard(repo, item.update.Ref); err != nil {
				_ = m.db.QuarantineGateRef(repo.ID, m.paths.RepoDir(repo.ID), item.update.Ref, item.update.New, item.update.New, "managed-gate-authority-unavailable")
				return fmt.Errorf("restore managed gate authority: %w", err)
			}
		}
	}
	return nil
}

// HandlePushReceived processes a push notification from the post-receive hook.
// It creates a run, sets up a worktree, and launches pipeline execution in the background.
func (m *RunManager) HandlePushReceived(ctx context.Context, params *ipc.PushReceivedParams) (string, error) {
	if params == nil {
		return "", fmt.Errorf("push notification: missing parameters")
	}
	if strings.TrimSpace(params.ReceiveSessionID) == "" || strings.TrimSpace(params.ReceiveCapability) == "" {
		return "", fmt.Errorf("push notification: authenticated receive capability is required")
	}
	repo, branch, err := m.receiveRepo(params.Gate, params.Ref)
	if err != nil {
		return "", err
	}
	active, err := m.db.VerifyReceiveSession(repo.ID, m.paths.RepoDir(repo.ID), params.ReceiveSessionID, params.ReceiveCapability)
	if err != nil {
		return "", err
	}
	if !active {
		return "", fmt.Errorf("push notification: receive session is no longer active")
	}
	lock, err := branchsync.AcquireBranchOwnershipLock(m.paths, repo, repo.WorkingPath, branch)
	if err != nil {
		return "", fmt.Errorf("acquire branch ownership lock: %w", err)
	}
	defer lock.Release()
	runID, err := m.reconcileReceiveReservationLocked(ctx, repo, params, lock, false)
	if err != nil {
		return "", err
	}
	return runID, nil
}

func (m *RunManager) receiveRepo(gatePath, ref string) (*db.Repo, string, error) {
	repoID, err := repoIDFromGatePath(gatePath)
	if err != nil {
		return nil, "", err
	}
	repo, err := m.db.GetRepo(repoID)
	if err != nil {
		return nil, "", fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return nil, "", fmt.Errorf("unknown repo for gate %s", gatePath)
	}
	if !samePath(gatePath, m.paths.RepoDir(repo.ID)) {
		return nil, "", fmt.Errorf("gate path does not match registered repository")
	}
	if !strings.HasPrefix(ref, "refs/heads/") {
		return nil, "", fmt.Errorf("unsupported receive ref %q", ref)
	}
	branch := branchFromRef(ref)
	if branch == "" || strings.Contains(branch, "..") {
		return nil, "", fmt.Errorf("invalid receive branch %q", branch)
	}
	return repo, branch, nil
}

func (m *RunManager) ensureManagedGateRefAvailable(repo *db.Repo, ref string) error {
	if repo == nil || !strings.HasPrefix(strings.TrimSpace(ref), "refs/heads/") {
		return nil
	}
	quarantine, err := m.db.GetGateRefQuarantine(repo.ID, m.paths.RepoDir(repo.ID), ref)
	if err != nil {
		return fmt.Errorf("check managed gate ref quarantine: %w", err)
	}
	if quarantine != nil {
		return fmt.Errorf("managed gate ref %s is quarantined after an unbound transition from %s to %s; reconcile it before accepting a receive", ref, quarantine.ExpectedHead, quarantine.ObservedHead)
	}
	return nil
}

func (m *RunManager) verifyManagedGateRefHead(repo *db.Repo, ref, current string, exists bool, expectedOld string, acquireAuthority bool) error {
	if repo == nil || !strings.HasPrefix(strings.TrimSpace(ref), "refs/heads/") {
		return nil
	}
	observed := db.NormalizeManagedGateHead(current)
	if !exists {
		observed = ""
	}
	quarantine, err := m.db.GetGateRefQuarantine(repo.ID, m.paths.RepoDir(repo.ID), ref)
	if err != nil {
		return err
	}
	if quarantine != nil {
		if db.NormalizeManagedGateHead(quarantine.ExpectedHead) != observed {
			return fmt.Errorf("managed gate ref %s is quarantined after an unbound transition from %s to %s; reconcile it before accepting a receive", ref, quarantine.ExpectedHead, quarantine.ObservedHead)
		}
		if err := m.db.ClearGateRefQuarantine(repo.ID, m.paths.RepoDir(repo.ID), ref); err != nil {
			return err
		}
	}
	managed, err := m.db.GetManagedGateRef(repo.ID, m.paths.RepoDir(repo.ID), ref)
	if err != nil {
		return err
	}
	if managed == nil {
		if !receiveOldMatches(current, exists, expectedOld) {
			if err := m.db.QuarantineGateRef(repo.ID, m.paths.RepoDir(repo.ID), ref, db.NormalizeManagedGateHead(expectedOld), observed, "unbound-or-unexpected-gate-ref"); err != nil {
				return err
			}
			return fmt.Errorf("managed gate ref %s is at %s instead of its authenticated old head %s; the ref was quarantined", ref, observed, expectedOld)
		}
		if err := m.db.SetManagedGateRefHead(repo.ID, m.paths.RepoDir(repo.ID), ref, db.NormalizeManagedGateHead(expectedOld)); err != nil {
			return err
		}
		if !acquireAuthority {
			return nil
		}
		return m.ensureManagedGateGuard(repo, ref)
	}
	if db.NormalizeManagedGateHead(managed.Head) != observed {
		if err := m.db.QuarantineGateRef(repo.ID, m.paths.RepoDir(repo.ID), ref, managed.Head, observed, "unbound-or-unexpected-gate-ref"); err != nil {
			return err
		}
		return fmt.Errorf("managed gate ref %s changed from journaled head %s to %s; the ref was quarantined", ref, managed.Head, observed)
	}
	if !acquireAuthority {
		return nil
	}
	return m.ensureManagedGateGuard(repo, ref)
}

func (m *RunManager) verifyManagedGateRefCommit(repo *db.Repo, ref, current string, exists bool, oldHead, newHead string) error {
	if repo == nil || !strings.HasPrefix(strings.TrimSpace(ref), "refs/heads/") {
		return nil
	}
	if err := m.ensureManagedGateRefAvailable(repo, ref); err != nil {
		return err
	}
	managed, err := m.db.GetManagedGateRef(repo.ID, m.paths.RepoDir(repo.ID), ref)
	if err != nil {
		return err
	}
	expectedOld := db.NormalizeManagedGateHead(oldHead)
	if managed == nil {
		if err := m.db.SetManagedGateRefHead(repo.ID, m.paths.RepoDir(repo.ID), ref, expectedOld); err != nil {
			return err
		}
	} else if db.NormalizeManagedGateHead(managed.Head) != expectedOld {
		if err := m.db.QuarantineGateRef(repo.ID, m.paths.RepoDir(repo.ID), ref, managed.Head, db.NormalizeManagedGateHead(current), "unbound-or-unexpected-gate-ref"); err != nil {
			return err
		}
		return fmt.Errorf("managed gate ref %s does not have its journaled old head %s; the ref was quarantined", ref, expectedOld)
	}
	observed := db.NormalizeManagedGateHead(current)
	if !exists {
		observed = ""
	}
	expectedNew := db.NormalizeManagedGateHead(newHead)
	if observed != expectedNew {
		if err := m.db.QuarantineGateRef(repo.ID, m.paths.RepoDir(repo.ID), ref, expectedNew, observed, "unbound-or-unexpected-gate-ref"); err != nil {
			return err
		}
		return fmt.Errorf("managed gate ref %s is at %s instead of committed head %s; the ref was quarantined", ref, observed, expectedNew)
	}
	return nil
}

func (m *RunManager) reconcileReceiveReservationLocked(ctx context.Context, repo *db.Repo, params *ipc.PushReceivedParams, lock *branchsync.BranchOwnershipLock, trustedStartup bool) (string, error) {
	branch := branchFromRef(params.Ref)
	var reservation *db.ReceiveReservation
	var err error
	if strings.TrimSpace(params.ReservationID) != "" {
		reservation, err = m.db.GetReceiveReservation(params.ReservationID)
		if reservation != nil && (reservation.RepoID != repo.ID || reservation.Branch != branch || reservation.Ref != params.Ref || reservation.OldSHA != params.Old || reservation.NewSHA != params.New) {
			return "", fmt.Errorf("receive reservation identity does not match the exact notification")
		}
		if reservation != nil && !trustedStartup && !reservation.MatchesSession(params.ReceiveSessionID, params.ReceiveCapability) {
			return "", fmt.Errorf("receive reservation session does not match the exact notification")
		}
	} else {
		reservation, err = m.db.GetPendingReceiveReservationForSession(repo.ID, branch, params.Ref, params.Old, params.New, params.ReceiveSessionID, params.ReceiveCapability)
	}
	if err != nil {
		return "", err
	}
	if reservation == nil {
		history, historyErr := m.db.GetLatestReceiveReservationForSession(repo.ID, branch, params.Ref, params.Old, params.New, params.ReceiveSessionID, params.ReceiveCapability)
		if historyErr != nil {
			return "", historyErr
		}
		if history != nil && history.State == db.ReceiveReservationPublished {
			if history.RunID != nil && *history.RunID != "" {
				return *history.RunID, nil
			}
			if isZeroObjectID(history.NewSHA) {
				return "", nil
			}
			return "", fmt.Errorf("published receive reservation has no run")
		}
		return "", fmt.Errorf("receive transaction evidence is missing; refusing unbound notification")
	}
	if reservation.State == db.ReceiveReservationPublished {
		if isZeroObjectID(reservation.NewSHA) {
			return "", nil
		}
		if reservation.RunID == nil || *reservation.RunID == "" {
			return "", fmt.Errorf("published receive reservation has no run")
		}
		return *reservation.RunID, nil
	}
	if reservation.State == db.ReceiveReservationReserved || reservation.State == db.ReceiveReservationPrepared {
		return "", fmt.Errorf("receive reservation is awaiting authoritative transaction evidence")
	} else if reservation.State != db.ReceiveReservationCommitted {
		return "", fmt.Errorf("receive reservation is no longer pending")
	}
	if !samePath(reservation.GatePath, m.paths.RepoDir(repo.ID)) {
		return "", fmt.Errorf("receive reservation target changed")
	}
	completeReservation := func(runID string) error {
		if trustedStartup {
			return m.db.CompleteReceiveReservation(reservation.ID, runID)
		}
		return m.db.CompleteReceiveReservationForSession(reservation.ID, runID, params.ReceiveSessionID, params.ReceiveCapability)
	}
	if run, err := m.db.GetRunByReceiveReservation(reservation.ID); err != nil {
		return "", err
	} else if run != nil {
		if err := completeReservation(run.ID); err != nil {
			return run.ID, err
		}
		return run.ID, nil
	}
	current, exists, err := gateReceiveRef(ctx, m.paths.RepoDir(repo.ID), reservation.Ref)
	if err != nil {
		return "", err
	}
	if err := m.verifyManagedGateRefHead(repo, reservation.Ref, current, exists, reservation.OldSHA, true); err != nil {
		return "", err
	}
	if isZeroObjectID(reservation.NewSHA) {
		if !exists {
			if err := completeReservation(""); err != nil {
				return "", err
			}
			return "", nil
		}
		return "", fmt.Errorf("receive ref %s is at %s, reservation expects deletion", reservation.Ref, current)
	}
	if !exists {
		return "", fmt.Errorf("receive ref %s is unavailable; reservation remains pending", reservation.Ref)
	}
	if exists && current == reservation.OldSHA {
		return "", fmt.Errorf("receive ref %s is still at its old head; reservation remains pending", reservation.Ref)
	}
	if exists && current != reservation.NewSHA {
		return "", fmt.Errorf("receive ref %s is at %s, reservation expects %s", reservation.Ref, current, reservation.NewSHA)
	}
	if !exists && !isZeroObjectID(reservation.OldSHA) {
		return "", fmt.Errorf("receive ref %s is unavailable; reservation remains pending", reservation.Ref)
	}
	runID, err := m.startRunWithIntentSourceLocked(ctx, repo, branch, reservation.NewSHA, reservation.OldSHA, "push", reservation.SkipSteps, reservation.Intent, db.RunIntentSourceAgent, lock, reservation.ID)
	if err != nil {
		return "", err
	}
	if err := m.releaseManagedGateGuard(repo.ID, reservation.Ref); err != nil {
		return runID, fmt.Errorf("release managed gate authority for active run: %w", err)
	}
	if err := completeReservation(runID); err != nil {
		return runID, err
	}
	return runID, nil
}

func (m *RunManager) reconcileReceiveReservations(ctx context.Context) {
	reservations, err := m.db.GetPendingReceiveReservations()
	if err != nil {
		slog.Warn("failed to list receive reservations", "error", err)
		return
	}
	for _, reservation := range reservations {
		repo, err := m.db.GetRepo(reservation.RepoID)
		if err != nil || repo == nil {
			slog.Warn("receive reservation cannot be reconciled", "reservation_id", reservation.ID, "error", err)
			continue
		}
		lock, err := branchsync.AcquireBranchOwnershipLock(m.paths, repo, repo.WorkingPath, reservation.Branch)
		if err != nil {
			continue
		}
		params := &ipc.PushReceivedParams{Gate: reservation.GatePath, Ref: reservation.Ref, Old: reservation.OldSHA, New: reservation.NewSHA, SkipSteps: reservation.SkipSteps, Intent: reservation.Intent}
		params.ReservationID = reservation.ID
		_, reconcileErr := m.reconcileReceiveReservationLocked(ctx, repo, params, lock, true)
		lock.Release()
		if reconcileErr != nil {
			slog.Warn("receive reservation remains pending", "reservation_id", reservation.ID, "error", reconcileErr)
			continue
		}
		if reservation.SessionID != "" {
			if err := m.db.RetireReceiveSession(reservation.SessionID); err != nil && !errors.Is(err, db.ErrReceiveSessionPending) {
				slog.Warn("recovered receive session remains active", "session_id", reservation.SessionID, "error", err)
			}
		}
	}
	sessions, err := m.db.GetActiveReceiveSessions()
	if err != nil {
		slog.Warn("failed to list active receive sessions", "error", err)
		return
	}
	for _, session := range sessions {
		if session.Phase != "admitted" && session.Phase != "aborted" {
			continue
		}
		if err := m.db.RetireReceiveSession(session.ID); err != nil && !errors.Is(err, db.ErrReceiveSessionPending) {
			slog.Warn("active receive session remains after startup reconciliation", "session_id", session.ID, "phase", session.Phase, "error", err)
		}
	}
}

func gateReceiveRef(ctx context.Context, gateDir, ref string) (string, bool, error) {
	exists, err := git.RefExistsBare(ctx, gateDir, ref)
	if err != nil {
		return "", false, fmt.Errorf("check receive ref %s: %w", ref, err)
	}
	if !exists {
		return "", false, nil
	}
	sha, err := git.ResolveRefBare(ctx, gateDir, ref)
	if err != nil {
		return "", false, err
	}
	return sha, true, nil
}

func receiveOldMatches(current string, exists bool, oldSHA string) bool {
	if isZeroObjectID(oldSHA) {
		return !exists
	}
	return exists && current == oldSHA
}

func isZeroObjectID(value string) bool {
	return (len(value) == 40 || len(value) == 64) && strings.Trim(value, "0") == ""
}

// HandleRerun creates a new run for the latest gate head on a branch. An
// explicit intent overrides the selected run. Otherwise an authoritative
// intent is inherited byte-for-byte; runs without one infer intent afresh.
func (m *RunManager) HandleRerun(ctx context.Context, repoID, branch, previousRunID string, skipSteps []types.StepName, intent string) (string, error) {
	repo, err := m.db.GetRepo(repoID)
	if err != nil {
		return "", fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return "", fmt.Errorf("unknown repo %s", repoID)
	}
	ownershipLock, err := branchsync.AcquireBranchOwnershipLock(m.paths, repo, repo.WorkingPath, branch)
	if err != nil {
		return "", fmt.Errorf("acquire branch ownership lock: %w", err)
	}
	defer ownershipLock.Release()

	gateDir := m.paths.RepoDir(repo.ID)
	ref := "refs/heads/" + branch
	key := managedGateGuardKey(repo.ID, ref)
	m.managedGateMu.Lock()
	_, hadGuard := m.managedGateGuards[key]
	m.managedGateMu.Unlock()
	if err := m.ensureManagedGateGuard(repo, ref); err != nil {
		return "", fmt.Errorf("acquire managed gate authority for rerun: %w", err)
	}
	keepGuard := false
	defer func() {
		if !hadGuard && !keepGuard {
			_ = m.releaseManagedGateGuard(repo.ID, ref)
		}
	}()
	quarantine, err := m.db.GetGateRefQuarantine(repo.ID, gateDir, "refs/heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("check managed gate ref quarantine: %w", err)
	}
	if quarantine != nil {
		return "", fmt.Errorf("managed gate ref refs/heads/%s is quarantined after an unbound transition from %s to %s; reconcile it before rerun", branch, quarantine.ExpectedHead, quarantine.ObservedHead)
	}
	headSHA, err := git.Run(ctx, gateDir, "rev-parse", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve gate head: %w", err)
	}
	managedGateRef, err := m.db.GetManagedGateRef(repo.ID, gateDir, "refs/heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("read managed gate ref journal: %w", err)
	}
	if managedGateRef == nil {
		return "", fmt.Errorf("managed gate ref refs/heads/%s has no authoritative head journal; reconcile it before rerun", branch)
	}
	if db.NormalizeManagedGateHead(managedGateRef.Head) != db.NormalizeManagedGateHead(headSHA) {
		if err := m.db.QuarantineGateRef(repo.ID, gateDir, "refs/heads/"+branch, managedGateRef.Head, headSHA, "unbound-or-unexpected-gate-ref"); err != nil {
			return "", fmt.Errorf("quarantine managed gate ref: %w", err)
		}
		return "", fmt.Errorf("managed gate ref refs/heads/%s changed from journaled head %s to %s; reconcile it before rerun", branch, managedGateRef.Head, headSHA)
	}

	runs, err := m.db.GetRunsByRepo(repoID)
	if err != nil {
		return "", fmt.Errorf("get runs: %w", err)
	}

	var latestForBranch *db.Run
	var matchingHead *db.Run
	for _, run := range runs {
		if run.Branch != branch {
			continue
		}
		if latestForBranch == nil {
			latestForBranch = run
		}
		if run.HeadSHA == headSHA {
			matchingHead = run
			break
		}
	}
	if latestForBranch == nil {
		return "", fmt.Errorf("no previous run for branch %s", branch)
	}
	selectedRun := latestForBranch
	if previousRunID != "" {
		selectedRun, err = m.db.GetRun(previousRunID)
		if err != nil {
			return "", fmt.Errorf("get selected run: %w", err)
		}
		if selectedRun == nil || selectedRun.RepoID != repoID || selectedRun.Branch != branch {
			return "", fmt.Errorf("selected run %s does not belong to repo %s branch %s", previousRunID, repoID, branch)
		}
	}

	baseSHA := latestForBranch.BaseSHA
	if matchingHead != nil {
		baseSHA = matchingHead.BaseSHA
	}

	intentSource := db.RunIntentSourceAgent
	if strings.TrimSpace(intent) == "" {
		intentSource = ""
		if selectedRun.Intent != nil && selectedRun.IntentSource != nil &&
			db.IsAuthoritativeRunIntentSource(*selectedRun.IntentSource) {
			// Do not normalize or regenerate this value. The selected run's
			// persisted bytes are the canonical acceptance criteria for the
			// replacement run.
			intent = *selectedRun.Intent
			intentSource = db.RunIntentSourceRerun
		}
	}

	keepGuard = true
	return m.startRunWithIntentSourceLocked(ctx, repo, branch, headSHA, baseSHA, "rerun", skipSteps, intent, intentSource, ownershipLock, "")
}

// fetchRunDefaultBranch fetches the trusted branch from the refreshed
// registration when it differs from the gate worktree's inherited origin. It
// updates only the run worktree's existing origin tracking ref and never
// rewrites clone or gate remote configuration. When the values agree after
// redaction, origin remains authoritative so embedded credentials retained in
// the gate can still authenticate without ever entering the database.
func fetchRunDefaultBranch(ctx context.Context, workDir string, repo *db.Repo) error {
	originURL, err := git.GetRemoteURL(ctx, workDir, "origin")
	if !repo.URLsVerified || (err == nil && safeurl.Redact(originURL) == repo.UpstreamURL) {
		return git.FetchRemoteBranch(ctx, workDir, "origin", repo.DefaultBranch)
	}
	return git.FetchRemoteBranchToRef(ctx, workDir, repo.UpstreamURL, repo.DefaultBranch, "refs/remotes/origin/"+repo.DefaultBranch)
}

// startRun creates a run, sets up a worktree, and launches pipeline execution.
// A non-empty intent is stamped onto the run as agent-supplied, so the intent
// step uses it instead of inferring from transcripts.
func (m *RunManager) startRun(ctx context.Context, repo *db.Repo, branch, headSHA, baseSHA, trigger string, skipSteps []types.StepName, intent string) (string, error) {
	return m.startRunWithIntentSource(ctx, repo, branch, headSHA, baseSHA, trigger, skipSteps, intent, db.RunIntentSourceAgent)
}

// startRunWithIntentSource is the common run-creation path. source is empty
// when no intent is supplied, RunIntentSourceAgent for a new explicit
// override, and RunIntentSourceRerun for inherited explicit intent.
func (m *RunManager) startRunWithIntentSource(ctx context.Context, repo *db.Repo, branch, headSHA, baseSHA, trigger string, skipSteps []types.StepName, intent, source string) (string, error) {
	return m.startRunWithIntentSourceLocked(ctx, repo, branch, headSHA, baseSHA, trigger, skipSteps, intent, source, nil, "")
}

func (m *RunManager) startRunWithIntentSourceLocked(ctx context.Context, repo *db.Repo, branch, headSHA, baseSHA, trigger string, skipSteps []types.StepName, intent, source string, heldLock *branchsync.BranchOwnershipLock, receiveReservationID string) (string, error) {
	branchRole := telemetryBranchRole(branch, repo.DefaultBranch)
	trackStartFailure := func(stage string) {
		telemetry.Track("run", telemetry.Fields{
			"action":      "start_failed",
			"trigger":     trigger,
			"branch_role": branchRole,
			"stage":       stage,
		})
	}

	if m.shuttingDown.Load() {
		trackStartFailure("daemon_shutdown")
		return "", fmt.Errorf("daemon is shutting down")
	}

	var ownershipLock *branchsync.BranchOwnershipLock
	if heldLock != nil {
		ownershipLock = heldLock
	} else {
		var err error
		ownershipLock, err = branchsync.AcquireBranchOwnershipLock(m.paths, repo, repo.WorkingPath, branch)
		if err != nil {
			trackStartFailure("branch_ownership_lock")
			return "", fmt.Errorf("acquire branch ownership lock: %w", err)
		}
		defer ownershipLock.Release()
	}

	lockKey := repo.ID + "/" + branch
	lockVal, _ := m.branchLocks.LoadOrStore(lockKey, &sync.Mutex{})
	branchMu := lockVal.(*sync.Mutex)
	branchMu.Lock()
	defer branchMu.Unlock()
	if strings.TrimSpace(receiveReservationID) == "" {
		pending, err := m.db.GetPendingReceiveReservationsForBranch(repo.ID, branch)
		if err != nil {
			trackStartFailure("receive_reservation")
			return "", fmt.Errorf("check receive reservations: %w", err)
		}
		if len(pending) > 0 {
			trackStartFailure("receive_reservation")
			return "", fmt.Errorf("receive reservation is pending for repository branch")
		}
	}

	// Best-effort only: a clone's remotes may change after init. Refresh the
	// registered URLs before constructing any run-owned Git operation, but keep
	// the exact prior repo value and continue when discovery, validation, or the
	// atomic database replacement fails. The reason is deliberately bounded and
	// URL-free so neither credentials nor sensitive remote material reach logs.
	if refreshed, _, refreshErr := gate.RefreshRepoURLs(ctx, m.db, repo); refreshErr != nil {
		slog.Warn("repository URL refresh skipped; continuing with existing registration", "repo_id", repo.ID, "reason", gate.ReasonForRefreshFailure(refreshErr))
	} else {
		repo = refreshed
	}

	// Cancel any active run for this repo+branch.
	m.cancelActiveRuns(repo.ID, branch)

	storedIntent := intent
	if source != db.RunIntentSourceRerun {
		storedIntent = strings.TrimSpace(storedIntent)
	}
	var runIntent *db.RunIntent
	if strings.TrimSpace(storedIntent) != "" {
		if source == "" {
			source = db.RunIntentSourceAgent
		}
		runIntent = &db.RunIntent{Summary: storedIntent, Source: source, Score: 1}
	}

	targetInputs, err := m.publicationTargetInputs(ctx, repo, branch, headSHA)
	if err != nil {
		trackStartFailure("publication_targets")
		return "", fmt.Errorf("enumerate publication targets: %w", err)
	}
	run, err := m.db.InsertRunWithIntentAndReceiveReservationAndTargets(repo.ID, branch, headSHA, baseSHA, runIntent, receiveReservationID, targetInputs)
	if err != nil {
		trackStartFailure("create_run")
		return "", fmt.Errorf("create run: %w", err)
	}

	// Create worktree from the gate bare repo.
	gateDir := m.paths.RepoDir(repo.ID)
	wtDir := m.paths.WorktreeDir(repo.ID, run.ID)
	if err := git.WorktreeAdd(ctx, gateDir, wtDir, headSHA); err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("create worktree: %s", err))
		trackStartFailure("create_worktree")
		return "", fmt.Errorf("create worktree: %w", err)
	}
	if err := git.CopyLocalUserIdentity(ctx, repo.WorkingPath, wtDir); err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("configure worktree git identity: %s", err))
		trackStartFailure("configure_worktree_identity")
		return "", fmt.Errorf("configure worktree git identity: %w", err)
	}
	// Fetch the trusted default branch and resolve it to an exact commit SHA
	// before any read. Reading the trusted config at this pinned SHA (rather
	// than the origin/<defaultBranch> remote-tracking ref) is what makes a
	// fetch failure fail closed: if the fetch errors or the ref does not
	// resolve, trustedSHA stays empty, loadTrustedRepoConfig returns nil, and
	// EffectiveRepoConfig drops the pushed branch's commands/agent. Without
	// the resolve, a stale origin/<defaultBranch> left in the shared bare
	// repo by a previous run could serve a trusted copy that the live default
	// branch has already removed - silently running stale shell.
	var trustedSHA string
	if repo.DefaultBranch != "" {
		fetchErr := fetchRunDefaultBranch(ctx, wtDir, repo)
		if fetchErr != nil {
			slog.Warn("failed to fetch default branch into worktree; trusted config disabled (commands/agent from pushed branch will be dropped)", "run_id", run.ID, "branch", repo.DefaultBranch, "error", fetchErr)
		} else if sha, err := git.ResolveRef(ctx, wtDir, "refs/remotes/origin/"+repo.DefaultBranch); err != nil {
			slog.Warn("failed to resolve fetched default-branch ref; trusted config disabled", "run_id", run.ID, "branch", repo.DefaultBranch, "error", err)
		} else {
			trustedSHA = sha
		}
	}

	// Track whether the background goroutine takes ownership of worktree cleanup.
	// If setup fails before the goroutine launches, we must clean up here.
	bgOwnsWorktree := false
	defer func() {
		if !bgOwnsWorktree {
			if rmErr := git.WorktreeRemove(context.Background(), gateDir, wtDir); rmErr != nil {
				slog.Warn("failed to remove worktree during setup cleanup", "path", wtDir, "error", rmErr)
			}
		}
	}()

	globalCfg, err := config.LoadGlobal(m.paths.ConfigFile())
	if err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("load config: %s", err))
		trackStartFailure("load_global_config")
		return "", fmt.Errorf("load global config: %w", err)
	}
	repoCfg, err := config.LoadRepo(wtDir)
	if err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("load config: %s", err))
		trackStartFailure("load_repo_config")
		return "", fmt.Errorf("load repo config: %w", err)
	}
	// SECURITY: load the code-executing selection fields (commands.* and
	// agent) from the trusted default-branch copy of .no-mistakes.yaml rather
	// than the pushed SHA. The worktree is checked out at headSHA (the
	// contributor's branch), so reading repoCfg above would honor a
	// contributor's commands/agent and let any pushed SHA run arbitrary shell
	// (sh -c) or pick the launched agent (incl. acp: targets) on the daemon
	// host with the maintainer's env (GH_TOKEN, SSH agent, ...).
	// EffectiveRepoConfig replaces commands + agent with the trusted
	// default-branch values unless the maintainer has explicitly opted in.
	//
	// allow_repo_commands is itself read ONLY from the trusted copy: a
	// contributor cannot self-enable it from the pushed branch. A readable
	// trusted tree with no config leaves the opt-in false and forces
	// commands/agent empty. An unreadable trusted tree aborts below.
	// SECURITY: a trusted-config fetch failure must abort, not silently disable
	// the disable_project_settings opt-out (see assertGateTrustedConfigReadable).
	if err := assertGateTrustedConfigReadable(ctx, wtDir, repo.DefaultBranch, trustedSHA); err != nil {
		m.db.UpdateRunError(run.ID, err.Error())
		trackStartFailure("trusted_config_unreadable")
		return "", err
	}
	trustedRepoCfg := loadTrustedRepoConfig(ctx, wtDir, trustedSHA, run.ID)
	allowRepoCommands := trustedRepoCfg != nil && trustedRepoCfg.AllowRepoCommands
	effectiveRepoCfg := config.EffectiveRepoConfig(repoCfg, trustedRepoCfg, allowRepoCommands)
	if allowRepoCommands {
		slog.Warn("allow_repo_commands is enabled on the default branch: honoring commands/agent from pushed branch", "run_id", run.ID, "branch", branch)
	} else if repoCfg.Commands != effectiveRepoCfg.Commands || repoCfg.Agent != effectiveRepoCfg.Agent || !agentListsEqual(repoCfg.Agents, effectiveRepoCfg.Agents) {
		// Surface the silent override so a maintainer who shipped a commands.*
		// or agent change on a feature branch understands why it did not run.
		// This is not an error: it is the secure default in action.
		slog.Info("repo commands/agent loaded from default branch, not pushed branch", "run_id", run.ID, "branch", branch, "default_branch", repo.DefaultBranch)
	}
	cfg := config.Merge(globalCfg, effectiveRepoCfg)

	// Create agent. In demo mode, skip resolution and use a no-op agent.
	var ag agent.Agent
	if steps.IsDemoMode() {
		ag = agent.NewNoop()
	} else {
		if err := cfg.ResolveAgent(ctx, exec.LookPath); err != nil {
			m.db.UpdateRunError(run.ID, err.Error())
			trackStartFailure("resolve_agent")
			return "", err
		}
		agents := cfg.Agents
		if len(agents) == 0 {
			agents = []types.AgentName{cfg.Agent}
		}
		created := make([]agent.Agent, 0, len(agents))
		for _, name := range agents {
			next, agErr := agent.NewWithOptions(name, cfg.AgentPathFor(name), cfg.AgentArgsFor(name), agent.Options{
				ACPRegistryOverrides:   cfg.ACPRegistryOverrides,
				DisableProjectSettings: cfg.DisableProjectSettings,
			})
			if agErr != nil {
				m.db.UpdateRunError(run.ID, fmt.Sprintf("create agent %s: %s", name, agErr))
				trackStartFailure("create_agent")
				return "", fmt.Errorf("create agent %s: %w", name, agErr)
			}
			// Steer every pipeline agent to keep writes inside the worktree and
			// avoid mutating system state (e.g. brew/Homebrew touching
			// /Applications), which triggers macOS App Management prompts.
			created = append(created, agent.WithSteering(next))
		}
		ag = agent.NewFallback(created)
		// Fail closed ONLY under the trusted opt-out: when the repo asked to
		// disable project settings, refuse any resolved harness that lacks a
		// verified suppression knob rather than launch it with the target repo's
		// project instructions loaded. When the repo did not opt out, every
		// adapter runs exactly as before (backward-compat).
		if cfg.DisableProjectSettings {
			if err := agent.EnsureGateNeutralized(ag); err != nil {
				m.db.UpdateRunError(run.ID, err.Error())
				trackStartFailure("gate_not_neutralized")
				return "", err
			}
		}
	}

	execSteps := m.steps()
	telemetry.Track("run", telemetry.Fields{
		"action":      "started",
		"trigger":     trigger,
		"agent":       string(cfg.Agent),
		"branch_role": branchRole,
		"step_count":  len(execSteps),
		"demo_mode":   steps.IsDemoMode(),
	})

	// Create executor with event broadcast.
	runCtx, cancel := context.WithCancelCause(context.Background())
	executor := pipeline.NewExecutor(m.db, m.paths, cfg, ag, execSteps, m.broadcast)
	executor.SetBranchRefUpdater(m.pipelineBranchRefUpdater(repo))
	executor.SetSkippedSteps(skipSteps)

	// Track executor.
	done := make(chan struct{})
	m.mu.Lock()
	m.executors[run.ID] = executor
	m.cancels[run.ID] = cancel
	m.dones[run.ID] = done
	m.mu.Unlock()

	// Background goroutine now owns worktree cleanup.
	bgOwnsWorktree = true

	// Launch pipeline in background.
	m.wg.Add(1)
	go func() {
		startedAt := time.Now()
		defer m.wg.Done()
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				errMsg := fmt.Sprintf("internal panic: %v", r)
				slog.Error("panic in pipeline goroutine", "run_id", run.ID, "panic", r)
				run.Status = types.RunFailed
				run.Error = &errMsg
				fields := telemetry.Fields{
					"action":      "finished",
					"trigger":     trigger,
					"agent":       string(cfg.Agent),
					"branch_role": branchRole,
					"status":      string(run.Status),
					"duration_ms": time.Since(startedAt).Milliseconds(),
					"step_count":  len(execSteps),
					"pr_created":  run.PRURL != nil && *run.PRURL != "",
				}
				if failedStep := telemetryFailedStepName(m.db, run.ID); failedStep != "" {
					fields["failed_step"] = failedStep
				}
				addRunPerformanceSummary(m.db, run.ID, fields)
				telemetry.Track("run", fields)
				if dbErr := m.db.UpdateRunErrorStatus(run.ID, errMsg, types.RunFailed); dbErr != nil {
					slog.Error("failed to update run after panic", "run_id", run.ID, "error", dbErr)
				}
			}
			cancel(nil)
			ag.Close()
			// Close subscriber channels for this run.
			m.closeSubscribers(run.ID)
			// Clean up worktree.
			if rmErr := git.WorktreeRemove(context.Background(), gateDir, wtDir); rmErr != nil {
				slog.Warn("failed to remove worktree", "path", wtDir, "error", rmErr)
			}
			// Remove tracking.
			m.mu.Lock()
			delete(m.executors, run.ID)
			delete(m.cancels, run.ID)
			delete(m.dones, run.ID)
			m.mu.Unlock()
		}()

		if err := executor.Execute(runCtx, run, repo, wtDir); err != nil {
			fields := telemetry.Fields{
				"action":      "finished",
				"trigger":     trigger,
				"agent":       string(cfg.Agent),
				"branch_role": branchRole,
				"status":      string(run.Status),
				"duration_ms": time.Since(startedAt).Milliseconds(),
				"step_count":  len(execSteps),
				"pr_created":  run.PRURL != nil && *run.PRURL != "",
			}
			if failedStep := telemetryFailedStepName(m.db, run.ID); failedStep != "" {
				fields["failed_step"] = failedStep
			}
			addRunPerformanceSummary(m.db, run.ID, fields)
			telemetry.Track("run", fields)
			slog.Error("pipeline failed", "run_id", run.ID, "error", err)
		} else {
			fields := telemetry.Fields{
				"action":      "finished",
				"trigger":     trigger,
				"agent":       string(cfg.Agent),
				"branch_role": branchRole,
				"status":      string(run.Status),
				"duration_ms": time.Since(startedAt).Milliseconds(),
				"step_count":  len(execSteps),
				"pr_created":  run.PRURL != nil && *run.PRURL != "",
			}
			addRunPerformanceSummary(m.db, run.ID, fields)
			telemetry.Track("run", fields)
			slog.Info("pipeline completed", "run_id", run.ID)
		}
	}()

	return run.ID, nil
}

func (m *RunManager) publicationTargetInputs(ctx context.Context, repo *db.Repo, branch, submitted string) ([]db.PublicationTargetInput, error) {
	if m == nil || repo == nil {
		return nil, fmt.Errorf("publication target ledger requires a repository")
	}
	targets, err := m.publicationTargetURLs(ctx, repo, branch)
	if err != nil {
		return nil, err
	}
	inputs := make([]db.PublicationTargetInput, 0, len(targets))
	ref := "refs/heads/" + strings.TrimPrefix(strings.TrimSpace(branch), "refs/heads/")
	for _, target := range targets {
		lineage, err := m.submissionTargetLineage(ctx, target.url, branch, submitted)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, db.PublicationTargetInput{
			TargetKind:        target.kind,
			TargetFingerprint: db.PublicationTargetFingerprint(target.url),
			Ref:               ref,
			TargetVersion:     repo.URLVersion,
			RequestLineage:    lineage,
		})
	}
	return inputs, nil
}

func (m *RunManager) submissionTargetLineage(ctx context.Context, target, branch, submitted string) (string, error) {
	provider := scm.DetectProviderContext(ctx, target)
	if provider != scm.ProviderGitHub && provider != scm.ProviderGitLab {
		return "none", nil
	}
	cmdFactory := func(cmdCtx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(cmdCtx, name, args...)
		shellenv.ConfigureShellCommand(cmd)
		return cmd
	}
	host := scm.ResolveHost(ctx, target)
	var verifier scm.SubmissionTargetLineageVerifier
	switch provider {
	case scm.ProviderGitHub:
		verifier = github.New(cmdFactory, func() bool { _, err := exec.LookPath("gh"); return err == nil }, host, github.HostPrefixedSlugForHost(target, host))
	case scm.ProviderGitLab:
		verifier = gitlab.New(cmdFactory, func() bool { _, err := exec.LookPath("glab"); return err == nil }, host, gitlab.ProjectPath(target))
	}
	if verifier == nil {
		return "", fmt.Errorf("submission-time publication lineage is unavailable for provider %s", provider)
	}
	refs, err := verifier.DiscoverSubmissionRequestRefs(ctx, strings.TrimPrefix(strings.TrimSpace(branch), "refs/heads/"), strings.TrimSpace(submitted))
	if err != nil {
		return "", fmt.Errorf("discover submission-time publication lineage: %w", err)
	}
	if len(refs) == 0 {
		return "none", nil
	}
	return strings.Join(refs, ","), nil
}

type publicationTargetURL struct {
	kind string
	url  string
}

func (m *RunManager) publicationTargetURLs(ctx context.Context, repo *db.Repo, branch string) ([]publicationTargetURL, error) {
	if m == nil || repo == nil {
		return nil, fmt.Errorf("publication target history requires a repository")
	}
	targets := make([]publicationTargetURL, 0, 6)
	indices := make(map[string]int)
	add := func(kind, raw string) {
		raw = strings.TrimSpace(raw)
		fingerprint := db.PublicationTargetFingerprint(raw)
		if raw == "" || fingerprint == "" {
			return
		}
		if index, ok := indices[fingerprint]; ok {
			if kind == "remote" {
				targets[index].url = raw
			}
			return
		}
		indices[fingerprint] = len(targets)
		targets = append(targets, publicationTargetURL{kind: kind, url: raw})
	}
	add("upstream", repo.UpstreamURL)
	add("fork", repo.ForkURL)
	remoteOutput, err := git.Run(ctx, repo.WorkingPath, "remote")
	if err != nil {
		return nil, fmt.Errorf("list working-clone remotes: %w", err)
	}
	for _, remote := range strings.Fields(remoteOutput) {
		if remote == gate.RemoteName {
			continue
		}
		fetchURLs, err := git.GetConfiguredRemoteURLs(ctx, repo.WorkingPath, remote)
		if err != nil || len(fetchURLs) != 1 || strings.TrimSpace(fetchURLs[0]) == "" {
			if err == nil {
				err = fmt.Errorf("remote %s has %d fetch URLs", remote, len(fetchURLs))
			}
			return nil, fmt.Errorf("read remote %s fetch URL: %w", remote, err)
		}
		pushURLs, err := git.GetConfiguredRemotePushURLs(ctx, repo.WorkingPath, remote)
		if err != nil || len(pushURLs) != 1 || strings.TrimSpace(pushURLs[0]) == "" {
			if err == nil {
				err = fmt.Errorf("remote %s has %d push URLs", remote, len(pushURLs))
			}
			return nil, fmt.Errorf("read remote %s push URL: %w", remote, err)
		}
		for _, raw := range []string{fetchURLs[0], pushURLs[0]} {
			add("remote", raw)
		}
	}
	return targets, nil
}

// addRunPerformanceSummary attaches the bounded per-run performance rollup
// to the terminal "run finished" event: low-cardinality counts only. The
// detailed per-invocation evidence (session keys, models, timings, tokens)
// stays in the local agent_invocations table and is never sent remotely.
func addRunPerformanceSummary(database *db.DB, runID string, fields telemetry.Fields) {
	summary, err := database.AgentInvocationSummaryForRun(runID)
	if err != nil {
		return
	}
	fields["agent_invocations"] = summary.Count
	fields["resumed_invocations"] = summary.Resumed
	fields["fallback_invocations"] = summary.Fallback
}

func telemetryBranchRole(branch, defaultBranch string) string {
	if branch == "" {
		return "unknown"
	}
	if defaultBranch != "" && branch == defaultBranch {
		return "default"
	}
	return "feature"
}

func telemetryFailedStepName(database *db.DB, runID string) string {
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		return ""
	}
	for _, step := range steps {
		if step.Status == types.StepStatusFailed {
			return string(step.StepName)
		}
	}
	return ""
}

// HandleRespond routes a user approval action to the executor for the given run.
func (m *RunManager) HandleRespond(runID string, step types.StepName, action types.ApprovalAction, findingIDs []string) error {
	return m.HandleRespondWithOverrides(runID, step, action, findingIDs, nil, nil)
}

// HandleRespondWithOverrides is like HandleRespond but also forwards user
// instructions and user-authored findings to the executor.
func (m *RunManager) HandleRespondWithOverrides(runID string, step types.StepName, action types.ApprovalAction, findingIDs []string, instructions map[string]string, addedFindings []types.Finding) error {
	m.mu.Lock()
	exec, ok := m.executors[runID]
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("no active executor for run %s", runID)
	}

	return exec.RespondWithOverrides(step, action, findingIDs, instructions, addedFindings)
}

// Shutdown cancels all active runs. Called during daemon shutdown to prevent
// orphaned goroutines from continuing agent calls and git operations.
func (m *RunManager) Shutdown() {
	m.shuttingDown.Store(true)

	m.mu.Lock()
	cancels := make(map[string]context.CancelCauseFunc, len(m.cancels))
	for id, cancel := range m.cancels {
		cancels[id] = cancel
	}
	m.mu.Unlock()

	for id, cancel := range cancels {
		cancel(fmt.Errorf("daemon shutting down"))
		slog.Info("cancelled run on shutdown", "run_id", id)
	}

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		slog.Warn("timed out waiting for runs to finish during shutdown")
	}
	m.managedGateMu.Lock()
	guards := m.managedGateGuards
	m.managedGateGuards = make(map[string]*branchsync.ManagedGateRefAuthority)
	m.managedGateMu.Unlock()
	for key, guard := range guards {
		if err := guard.Release(); err != nil {
			slog.Warn("failed to release managed gate authority", "ref_key", key, "error", err)
		}
	}
}

// HandleCancel stops an active run and propagates cancellation to the executor.
func (m *RunManager) HandleCancel(runID string) error {
	m.mu.Lock()
	cancel, ok := m.cancels[runID]
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("no active run %s", runID)
	}

	cancel(fmt.Errorf(types.RunCancelReasonAbortedByUser))
	return nil
}

// cancelActiveRuns cancels any in-progress runs for the given repo+branch
// and waits for their goroutines to finish before returning, preventing
// concurrent pushes to upstream.
// The cancellation cause is propagated to the executor via context.Cause,
// which uses it as the run's error message in the DB.
func (m *RunManager) cancelActiveRuns(repoID, branch string) {
	runs, err := m.db.GetRunsByRepo(repoID)
	if err != nil {
		slog.Error("failed to query active runs for cancellation", "repo", repoID, "branch", branch, "error", err)
		return
	}

	var toWait []chan struct{}
	for _, run := range runs {
		if run.Branch != branch {
			continue
		}
		if run.Status != types.RunPending && run.Status != types.RunRunning {
			continue
		}

		m.mu.Lock()
		cancel, ok := m.cancels[run.ID]
		done := m.dones[run.ID]
		m.mu.Unlock()
		if !ok {
			continue
		}

		cancel(fmt.Errorf(types.RunCancelReasonSuperseded))
		slog.Info("cancelled active run", "run_id", run.ID, "repo_id", repoID, "branch", branch)
		if done != nil {
			toWait = append(toWait, done)
		}
	}

	timeout := time.After(30 * time.Second)
	for _, done := range toWait {
		select {
		case <-done:
		case <-timeout:
			slog.Warn("timed out waiting for cancelled runs to finish")
			return
		}
	}
}
