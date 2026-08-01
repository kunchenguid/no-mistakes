# Event-stream overflow and reconciliation evidence

Focused race-enabled verification exercised the daemon mailbox, wire transport,
AXI reconciliation, TUI snapshot application, database-before-event ordering,
and the bounded on-demand fix-review diff RPC.

## Daemon pressure behavior

```text
64 queued state frames + 1 overflow transition
delivered_state_frames=64
gap_high_water=65
final_daemon_revision=65

10,000 log events
queued=64
queued_bytes=9600
dropped_activity=9936
gap=false

50,000 state transitions after filling the state queue
queued=64
queued_bytes=8192
coalesced_state=50000
emitted_gap_frames=1

consumer after authoritative reconciliation
authoritative_revision=67
applied_revision=67
final_ci_status=completed
```

This directly demonstrates both bounds and the loss contract: activity may be
dropped, state pressure coalesces to one high-water `stream_gap`, and stale
queued deltas cannot regress the authoritative result.

## Consumer and wire behavior

```text
AXI: stream_gap_rev=71
AXI resulting_status=completed
AXI operations=subscribe,reconcile,reconcile

wire frames=[step_completed stream_gap run_completed]
wire terminal_revision=10
wire findings_payload_bytes=65536

get_step_diff returned_bytes=524288
get_step_diff cap_bytes=524288
get_step_diff truncated=true
```

AXI subscribed before its initial read and spent exactly one additional
authoritative read on the gap. The transport delivered the gate, gap, and
terminal frames in order. The previously unbounded fix-review diff is capped at
512 KiB and marked truncated.

## End-user TUI result

After a coalesced gap at revision 42 invalidated stale readiness, the live title
changed from the green checks-passed state to:

```text
⠋ CI - feature/foo
```

A terminal `failed` snapshot delivered only through a gap completed the view at
revision 50 and rendered:

```text
╭─ Pipeline ─────────────────────────────────────────────────────────────╮
│                                                                failed │
╰────────────────────────────────────────────────────────────────────────╯

✗ Pipeline failed

  q quit  ? help  y yolo  r rerun
```

The evidence is textual because the affected UI is an ANSI terminal interface
and the focused model harness renders its actual `View()` string without a
browser or image surface. The rendered terminal output is included above
instead of substituting a DOM snapshot.

## Commands

```text
go test -race ./internal/daemon -run 'TestMailbox_(SubscriptionOpensWithGap|LifecycleSurvivesFullActivityBuffer|StatePressureFoldsInsteadOfEvictingState|TerminalCompletionCannotBeHidden|FindingsAndApprovalCannotBeHidden|ActivityIsBoundedDroppableAndRaisesNoGap|ByteCeilingBindsBeforeCountCeiling|GapCoalescesAndNeverGrows|GapDrainsAheadOfQueuedPayload|PublisherNeverBlocksOnWedgedSubscribers|StaleDeltasCannotRegressAfterSnapshot|ReconnectConvergesAtCurrentRevision|ConcurrentChurnIsRaceFree|ManySimultaneousTransitionsCollapseToOneGap|StateRevisionsAdvanceInEnqueueOrder)$' -v

go test -race ./internal/cli ./internal/tui -run 'TestRunReconciler_(StreamGapForcesOneAuthoritativeRead|UnknownEventTypeIsTreatedAsStateBearing)|TestTUIOverflow_(GreenTitleClearsFromAuthoritativeSnapshot|StaleQueuedDeltaCannotRegressAfterSnapshot|NewerDeltaAfterSnapshotStillApplies|TerminalSnapshotThroughGapCompletesTheView|DroppedStreamResubscribesAndResetsRevision|ReconnectAttemptsAreBounded|CompletedGapThenCloseConvergesWithoutReconnect|ReconciliationRequestsCoalesce|UnknownStateEventRequestsAuthoritativeReconciliation)$' -v

go test -race ./internal/ipc ./internal/daemon -run 'Test(ClassOf|SubscribeBoundedFramesDeliverThroughTerminalEvent|SubscribeOversizedFrameEndsTheStreamAndHidesLaterEvents|StepDiff_ReturnsTheWorktreeDiffOnDemand|StepDiff_BoundsAnOversizedDiff|RunSnapshot_SamplesRevisionBeforeReadingSoConcurrentChangesStillApply|RunSnapshot_CompletedRunRetainsTerminalRevisionUntilEviction)$' -v

go test -race ./internal/ipc -run 'TestClassOfPartitionsEventsByLossTolerance|TestClassOfUnknownEventFailsSafeToState|TestStreamGapAndStateRevRoundTrip' -v

go test -race ./internal/pipeline -run 'TestExecutor_(StateEventsAreEmittedAfterTheirDatabaseWrite|SkippedStepEventsAlsoFollowTheirDatabaseWrite|ApprovalPersistenceFailureDoesNotPublishOrWaitAtGate)' -v
```
