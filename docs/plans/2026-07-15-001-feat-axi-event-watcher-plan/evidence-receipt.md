# Evidenz-Receipt

| Plan item | Required proof | Evidence produced | Status | Claim allowed | Residual action |
|---|---|---|---|---|---|
| R1-R3, U1 | Öffentlicher CLI-Contract und Flag-Validierung | `TestParseWatchUntilDefaultsToAttentionAndRejectsUnknown`; Binary-Help-Smoke | verified | Öffentlicher Watch-Contract | none |
| R4-R5, U2-U3 | Autoritative Event-Beobachtung, Race- und Quiet-Latch-Fälle | `TestWatchQuietDelayUsesNearestActiveStep`; `TestRenderWatchResultBoundsGateFindings`; bestehende IPC-Subscription-E2E | verified | Ereignisse wecken, der aktuelle State entscheidet; Ausgabe bleibt begrenzt | none |
| R6, U2a | Unterbrechbarer Handshake und keine Pipeline-Mutation | `TestSubscribeContextCancelsDuringHandshake`; Signalpfad per Kontext entkoppelt | verified | Abbruch beendet nur Watch-Connection | none |
| R7, U3 | Begrenzter, datensparsamer TOON- und Telemetrie-Contract | Gate-Test mit elf Findings, Output enthält zehn plus Trunkierungsmarker | verified | Begrenzte Watch-Entscheidungsausgabe | none |
| R8, U4 | Generierte Skill-, Hilfe- und Dokumentationssynchronität | `make skill`; `make lint`; `make docs-build` | verified_local | Watch- und Supervisor-Guidance sind versionsgleich | Sichtbarer TUI-Hook-Lauf bleibt U7-Gate |
| U5 | Lokaler Daemon-/CLI-Harness | `make e2e` mit einmaligem Timing-Flake, gezieltes `TestUserJourney/claude` grün; `./bin/no-mistakes axi watch --help` | partial | Bestehende Daemon-/IPC-E2E und neuer CLI-Einstieg funktionieren separat | Kein Watch-spezifischer Daemon-Harness |
| U6 | Supervisor-Registrierung und Hook-Parser | `go test ./internal/cli ./internal/supervision ./internal/ipc`; Windows-Kompilierung | verified_local_pre_replacement | Lokale, opt-in Run-/Arbeitskopiebindung und Parser sind im Worker-Vorgängerstand implementiert | Hook-native Fortsetzung ersetzt den früheren Worker-/Resume-Pfad und muss Bindung sowie Parser danach erneut beweisen |
| U7 | Hook-native Fortsetzung, Heartbeat-Rearming und begrenzte Stale-Geduld | `TestStorePrepareHandoffDeduplicatesTurnAndFingerprint`; Supervisor-Klassifikation; Budget 1/4/6; `go test -race ./...`; `make e2e`; `make lint`; `make build`; `make docs-build` | verified_local_pending_live | Direkter Hook-Pfad ohne Worker/`codex exec resume`, begrenzte statische Reason-Codes und lokale Pausenlogik | Echter sichtbarer Hook-Lauf mit geprüftem Timeout bleibt erforderlich |
| R16, U7-Vor-Gate | Zwei aufeinanderfolgende Stop-Hook-Fortsetzungen derselben Sitzung | 2026-07-16: Codex CLI 0.144.5, einmaliger Probe, zwei statische Block-Signale, dritter Stop endet regulär | verified_live_local_noninteractive | Zwei aufeinanderfolgende Hook-Fortsetzungen derselben Session; `stop_hook_active` ist beim zweiten legitimen Turn bereits gesetzt | Sichtbaren TUI-/Chat-Nachweis vor Abschluss von U7 durchführen; keine IDs oder Chat-Inhalte im Receipt behalten |
| U7 Operator-Preflight | Installierter Stop-Hook, Hash und Timeout >= 360 Sekunden | Noch nicht durchgeführt | pending_manual_preflight | Keine sichtbare Dauerbetreuungs-Behauptung | Vor TUI-Abnahme datensparsame Receipt-Werte erfassen; keine Hook-Konfiguration automatisch ändern |
| Definition of Done | Lint, Race-Tests, E2E, Build | `make lint`; `make test`; `make e2e`; `make build`; `make docs-build`; `git diff --check` | pending_live_gate | Lokale Implementierungsreife für Watcher und Supervisor | Echter Smart-Commit/No-Mistakes-Lauf auf einem committed Branch und sichtbarer Hook-Resume |

Claim: workflow_completion: AXI Watcher plus Hook-native Codex-Supervisor
Evidence class: verified_local_pending_live
Evidence: Fokussierte Supervisor-/Registrierungsregressionen einschließlich Stale-Budgets 1/4/6, vollständige Race-Suite, E2E, Lint, Build, Skill- und Docs-Build.
Proves: Der direkte Hook-Pfad, Heartbeat-Rearming, aktuelle Supervisor-Guidance und die lokale Pausenlogik sind ohne einen zweiten Codex-Prozess implementiert und lokal validiert.
Does not prove: Dass eine sichtbare Codex-TUI-/Chat-Session mit der installierten lokalen Hook-Konfiguration über mehrere echte AXI-Ereignisse fortgesetzt wird.
Residual risk: Vor einer Dauerbetreuungs-Behauptung den manuell installierten Hook, seinen Hash und Timeout >= 360 Sekunden prüfen und einen sichtbaren Lauf dokumentieren.
