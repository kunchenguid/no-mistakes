---
artifact_contract: ce-unified-plan/v1
artifact_readiness: implementation-ready
product_contract_source: ce-plan-bootstrap
execution: code
---

# AXI-Ereignisbeobachter - Plan

## Zusammenfassung

`no-mistakes axi watch` ergänzt die vorhandene AXI-Oberfläche um eine
ereignisgesteuerte Beobachtung eines **explizit angegebenen** Runs. Der Befehl
nutzt den bestehenden daemonseitigen Run-Eventstream und liefert eine begrenzte
TOON-Zustandsaufnahme zurück, sobald externe Aufmerksamkeit nötig ist oder der
Run endet. Er startet, beantwortet, repariert, rebased, wiederholt oder bricht
keinen Run ab.

Der Watcher ist der Ereignissensor eines opt-in Codex-CLI-Supervisors. Endet
ein Codex-Turn, bindet ein explizit installierter Codex-Stop-Hook dessen
Session-ID an die gerüstete Run-ID und beobachtet den Run selbst bis zum
nächsten relevanten Ereignis. Der Hook gibt dann das offizielle Stop-Hook-
Fortsetzungssignal zurück. Codex setzt dadurch dieselbe Sitzung sichtbar fort;
ein separater `codex exec resume`-Prozess ist weder nötig noch erwünscht. Nur
ein echtes CEO-Gate übergibt die Kette an Simon.

## Problemrahmen

`axi run` und `axi respond` warten bereits synchron bis zum nächsten Gate,
`checks-passed` oder Endzustand. Für einen bereits laufenden Run gibt es aber
nur wiederholtes `axi status`-Polling. Wenn Smart Commit den Aufruf in den
Hintergrund legt, verliert die aktive Codex-Runde dessen Abschluss und kann
den Lauf nicht autonom weiterführen.

Die vorhandene IPC-Subscription ist die richtige Grundlage. Sie wird von der
TUI verwendet, ist aber noch nicht als AXI-Contract für Agent-Supervisoren
angeboten. Die aktuelle Codex-CLI liefert jedoch einen nativen Stop-Hook-
Fortsetzungsvertrag: Ein Hook kann nach einem relevanten Ereignis eine neue
Fortsetzung derselben Sitzung verlangen. Damit ist ein separater
Sitzungs-Reentry-Prozess überflüssig.

## Produktvertrag

### Produktintention

Ein explizit aktivierter Codex-CLI-Supervisor soll einen laufenden
No-Mistakes-Run ohne Status-Polling bis zum terminalen Zustand begleiten und
dieselbe Codex-Session nur bei einer echten Run-Änderung fortsetzen. Der
Supervisor beantwortet keine Gates selbst und darf keine CEO-Entscheidung
umgehen.

### User Story

Als Nutzer eines agentengesteuerten Codex-CLI-Laufs möchte ich einen bekannten
Run ereignisgesteuert überwachen lassen, damit Codex technische Arbeit selbst
weiterführt und mich nur bei einer echten Entscheidung informiert, statt dass
ich regelmäßig nach dem Status fragen muss.

### Ziel und Nutzerreise

Smart Commit kennt die Run-ID und aktiviert `axi supervise arm --run <id>`.
Der Codex-Stop-Hook erhält beim Ende des aktuellen Turns die Session-ID,
übernimmt ausschließlich diese gerüstete Run-ID und beobachtet sie über eine
direkte, read-only Daemon-Subscription. Er ruft nicht `axi watch` und startet
keinen zweiten Codex-Prozess. Für einen neuen technischen Fortschritt, einen
Heartbeat oder einen terminalen Zustand speichert er den Stoppgrund und gibt
ein begrenztes offizielles Stop-Hook-Signal zurück. Codex setzt dadurch
dieselbe Sitzung mit einem Ereignis-Prompt fort. Die fortgesetzte Runde
entscheidet innerhalb des bestehenden Smart-Commit-Rahmens über technische
Antworten und ruft gegebenenfalls `axi respond` auf.

Nach einem fortgesetzten Turn prüft der Stop-Hook autoritativ: Läuft der Run
wieder, beginnt eine neue Watch-Phase; steht er an einem CEO-Gate, wird die
Supervision auf `awaiting_user` gesetzt und der Hook lässt den Turn anschließend
wirklich enden. Die automatische Kette pausiert, bis dieselbe Session Simons
Antwort erhalten hat und danach erneut endet; erst dann darf die nächste
Watch-Phase beginnen. Ist der Run terminal, wird die Registrierung als
abgeschlossen markiert. Quiet ist kein eigener Supervisionszustand; die
direkte Subscription wartet stattdessen bis zum nächsten Ereignis oder zur
festen, nach jedem sichtbaren Handoff neu geplanten Heartbeat-Frist.

`--until terminal` bleibt eine rein beobachtende Variante. Sie gibt nur den
terminalen Snapshot aus und beobachtet Gate, `checks-passed` und Quiet ohne
Zwischen-Ausgabe weiter. Das ist nicht der Codex-Supervisionsmodus.

### Admin-/Operator-Journey

Nicht anwendbar: Der Change ergänzt nur den agentenseitigen Read-Contract und
schafft keine neue Bedienoberfläche, Rolle oder Administrationsaktion.

### Anforderungen

- R1: `axi watch` verlangt immer `--run <id>` und fällt niemals still auf
  aktuellen Branch oder letzten Run zurück.
- R2: `--until attention` ist der Default und beendet die Beobachtung bei
  Gate, `checks-passed`, Quiet-Signal oder terminalem Runzustand.
- R3: `--until terminal` beobachtet durch Gate, `checks-passed` und Quiet
  hindurch, ohne den Run zu verändern oder Zwischen-Snapshots auszugeben, bis
  zum terminalen Zustand.
- R4: Jeder erfolgreiche Beobachtungsabschluss enthält einen begrenzten TOON-Snapshot mit eindeutigem
  Stoppgrund und der Grenze `supervision: active_agent_required`; ein Gate enthält
  zusätzlich eine für den Watcher begrenzte Gate-Darstellung und ihre
  Handlungsanweisungen.
- R5: Stream-Ereignisse sind nur Aufwecker. Vor jeder Entscheidung wird der
  aktuelle, autoritative Runzustand erneut geladen, damit verlorene,
  verspätete oder lückenhafte Events keinen falschen Zustand behaupten.
- R6: SIGINT, SIGTERM und Kontextabbruch trennen ausschließlich die
  Beobachtung, geben einen begrenzten `interrupted`-Snapshot mit Exitcode 130
  aus und senden niemals `axi abort` oder eine andere Mutation. Ein
  nicht-terminal geschlossener Stream wird nach dem finalen Read als
  `stream-interrupted` mit Exitcode 1 ausgegeben; schlägt dieser finale Read
  fehl, gilt der bestehende strukturierte AXI-Betriebsfehler.
- R7: Der Watcher bleibt datensparsam: kein Rohlog, Diff, Prompt oder Event-
  Content auf stdout oder in Telemetrie. Er folgt dem bestehenden Read-Surface-
  Telemetrie-Gate ohne Pageview.
- R8: Skill, punktnahe AXI-Hilfe und Agent-Dokumentation erklären dieselbe
  Grenze: `axi watch` ist nur der Read-only-Ereignissensor; echte autonome
  Fortsetzung verlangt den opt-in Codex-Supervisor.
- R9: `axi supervise arm --run <id>` registriert exakt einen aktiven Run für
  die aktuelle Arbeitskopie, enthält keine Session-ID und startet keinen
  Worker. Es darf keine Pipeline-Mutation auslösen. Seine begrenzte Ausgabe
  benennt sichtbar die Betriebsgrenze: Während einer betreuten Run-Kette darf
  nur eine Codex-Session je Arbeitskopie aktiv sein.
- R10: `axi codex-hook` akzeptiert ausschließlich Codex-Stop-Hook-JSON über
  stdin. Es verlangt `Stop`, nichtleere `session_id` und `turn_id` sowie einen
  kanonischen `cwd`; `stop_hook_active` wird nur als Kontext geparst. Vor
  Claim und vor jedem Handoff prüft es, dass der aktuelle Repository-Root zum
  gespeicherten `repo_id` passt und `run.repo_id == reg.repo_id` gilt.
  Fehlender, fehlerhafter oder nicht passender Input bleibt ohne
  Fortsetzungssignal; eine festgestellte Bindungsabweichung pausiert die
  beanspruchte Registrierung mit einer begrenzten Diagnose.
- R11: Der Hook verwendet eine direkte read-only Run-Subscription und gibt bei einem
  neuen technischen Ereignis, Heartbeat oder terminalen Ergebnis genau ein
  gültiges Stop-Hook-Fortsetzungssignal `{ "decision": "block", "reason":
  "..." }` zurück. `reason` stammt ausschließlich aus einer festen Allowlist
  von Ereigniscodes; Finding-, Fehler-, PR-, Log-, Run- und Sessiontexte
  gelangen nie in diesen Codex-Kanal. Er startet weder Worker noch
  `codex exec resume`, liest keine Session-Dateien und erzeugt keine
  Retry-Stürme.
- R12: Nur ein `ask-user`-/CEO-Gate pausiert automatische Hook-Fortsetzungen,
  markiert die Registrierung `awaiting_user` und übergibt den sichtbaren Chat
  an Simon. Ein rein technisches Gate wird einmal an dieselbe Sitzung
  übergeben; `checks-passed` ist ein eigener Merge-/Handoff-Zustand. Nur
  dieselbe Session darf nach Simons Antwort und einem späteren Turn-Ende die
  nächste Watch-Phase anfügen. Ein terminaler Run wird als abgeschlossen
  markiert.
- R14: Die Registrierung speichert `last_handoff_turn_id`, Ereignisfingerprint
  und `next_heartbeat_at`. Derselbe `(turn_id, Ereignisfingerprint)` darf kein
  zweites Fortsetzungssignal erzeugen. `stop_hook_active` allein sperrt keine
  neue, legitime Fortsetzung: Der R16-Probe hat gezeigt, dass es beim zweiten
  Turn derselben Session bereits gesetzt ist. Die konkrete Zustandsregel
  unterscheidet deshalb einen neuen Turn mit neuem Ereignis oder neuer
  Heartbeat-Generation von einer Wiederholung desselben Turns. Der Hook
  schreibt Turn, Fingerprint und Zielzustand unter demselben exklusiven Store-
  Claim, bevor er sein einziges JSON-Signal auf stdout ausgibt.
- R15: Der Supervisor verwendet eine eigene Heartbeat-Frist von fünf Minuten,
  die ab jedem sichtbaren Handoff neu beginnt und kürzer als der manuell
  konfigurierte Stop-Hook-Timeout sein muss. Der Hook wartet daher nie länger
  als eine Heartbeat-Frist ohne sichtbare Fortsetzung. Die Frist ist fest und
  nicht konfigurierbar.
- R16: Vor U7 beweist ein ausdrücklich freigegebener, kurzlebiger
  `live_local`-Probe zwei aufeinanderfolgende `decision: block`-
  Fortsetzungen derselben sichtbaren Codex-Session. Er hält nur Codex-Version,
  Hook-Konfigurationshash, Zeitpunkte sowie die Strukturbeziehung von
  `turn_id` und `stop_hook_active` fest, niemals deren Werte, Session-IDs,
  Run-IDs oder Chat-Inhalte. Ohne diesen Beweis bleibt U7 ungeplant und der
  direkte Hook-Pfad geht zurück in die Architekturentscheidung.
- R17: `supervision_max_stale_heartbeats` begrenzt sichtbare Heartbeats ohne
  autoritativen Run-Fortschritt auf einen ganzzahligen Wert von 1 bis 6;
  Standard und Fallback bei ungültiger Konfiguration sind 4. Nach dem Budget
  liefert der Hook genau einen Stale-Handoff und pausiert bis zu einer
  bewussten neuen Aktion. `checks-passed` liefert genau einen Merge-Handoff
  und geht danach auf `awaiting_merge_result`; Watcher-Transport-, Parse- oder
  Betriebsfehler liefern höchstens einen technischen Fehler-Handoff und gehen
  danach ebenfalls fail-closed auf Pause.
- R13: Hook-Installation und lokale Benachrichtigung bleiben explizites Opt-in;
  ein No-Mistakes-Update darf weder `~/.codex/hooks.json` noch globale
  Benachrichtigungs- oder Codex-Settings still ändern.

### Nicht im Umfang

- Kein globaler Autostart, Cron-Job oder stiller Eingriff in Codex-Settings.
- Keine automatische Gate-Antwort außerhalb des bestehenden, ausdrücklich
  zustimmungsgebundenen `--yes`-Pfads.
- Keine neue Daemon- oder Datenbank-Persistenz und keine neue menschenorientierte
  No-Mistakes-TUI.
- Keine Zusage, dass ein nicht installierter oder nicht vertrauenswürdiger Hook
  einen Codex-Turn fortsetzt.
- Keine technische Owner-Handshake-Schicht für mehrere parallele
  Codex-Sessions in derselben Arbeitskopie. Das ist im ersten Schnitt eine
  explizite Betriebsgrenze, keine behauptete Isolation.

## Technische Entscheidungen

| Entscheidung | Begründung |
| --- | --- |
| Bestehendes `ipc.Subscribe` wiederverwenden | Der Daemon streamt bereits run-scoped Ereignisse; ein zweiter Polling- oder Daemonvertrag wäre unnötige Doppelarbeit. |
| Event als Aufwecker, erneuter Run-Read als Wahrheit | Subscriptions enthalten keine Startaufnahme und können bei langsamen Konsumenten Events verlieren. |
| Explizite Run-ID | Ein Supervisor darf niemals versehentlich einen anderen oder den letzten Run beobachten. |
| `attention` als Default | Gates verlangen externe Entscheidung; ein ausschließliches `--until-terminal` würde einen wartenden Run wie autonome Arbeit wirken lassen. |
| Quiet als Hinweis, nicht als Fehler | Quiet bedeutet fehlende Aktivität, nicht Berechtigung zum Abbruch, Re-run oder zur Reparatur. |
| Read-only bedeutet auch keinen Daemon-Start | `watch` abonniert nur einen bereits laufenden Daemon. Er darf weder `EnsureDaemon` noch Stale-Recovery, Resume oder Worktree-Cleanup auslösen. |
| Terminal-Modus hat keinen Zwischen-Stream | Ein einzelner terminaler Snapshot vermeidet einen zweiten Streaming-Parser und bleibt als passive Diagnose bewusst klein. |
| Codex-Session-ID kommt nur aus dem Stop-Hook | Die laufende AXI-CLI darf keine Session-Dateien erraten oder eine fremde Codex-Session übernehmen. |
| Registrierung vor Beobachtung | `arm` schreibt nur eine kleinste lokale Registrierung; erst ein verifizierter Stop-Hook darf sie mit seiner Session-ID übernehmen. |
| Eine Codex-Session je Arbeitskopie | Die armende CLI kennt keine Session-ID. Der erste Schnitt dokumentiert deshalb die Einzel-Session-Betriebsgrenze sichtbar, statt eine zweite globale Owner-Registry zu bauen oder Cross-Session-Isolation zu behaupten. |
| Hook-native Fortsetzung | Ein Stop-Hook darf Codex mit dem dokumentierten `decision: block`-Signal selbst fortsetzen; ein zweiter Codex-Prozess wäre komplexer und nicht sichtbar in derselben Unterhaltung. |
| Ereignis plus Heartbeat statt Polling-Cron | Der Hook wartet auf AXI-Events und auf eine ab dem letzten Handoff neu geplante Heartbeat-Frist. |
| Begrenzte, konfigurierbare Geduld | Fünf Minuten bleiben fest; nur die Zahl der Heartbeats ohne Run-Fortschritt ist mit `supervision_max_stale_heartbeats` zwischen 1 und 6 einstellbar. Standard 4 gibt Updates nach 5, 10, 15 und 20 Minuten; danach pausiert ein einmaliger Stale-Handoff statt einer Endlosschleife. |
| CEO-Gate pausiert die Automatik | Nur ein `ask-user`-Gate blockiert die Automatik; technische Gates werden derselben Sitzung einmal übergeben, `checks-passed` folgt dem Smart-Commit-Mergevertrag. |
| Probe vor Deduplizierungsregel | Der Zwei-Handoff-`live_local`-Probe hat gezeigt: der zweite legitime Turn hat eine neue Turn-Beziehung, aber bereits `stop_hook_active: true`; eine spätere Wiederholung kann denselben Turn tragen. Deduplizierung bindet deshalb Turn und Ereignis zusammen, nicht `stop_hook_active` allein. |

## Hochrangiges Design

```text
Smart Commit / active Codex turn
        |
        +--> axi supervise arm --run <id>
        |
        v
Codex Stop-Hook receives session_id
        |
        +--> atomically claim matching armed registration
        v
same Stop hook: axi watch --run <id> --until attention
        |
        +--> authoritative snapshot via IPC + rearmed Heartbeat deadline
        |
        +--> official Stop-hook continuation JSON
                    |
                    +--> active run: next Stop starts next watch phase
                    +--> CEO gate: awaiting_user, then real user handoff
                    +--> terminal: final visible handoff, then finalize
```

## Implementierungseinheiten

### Aktuelle Umsetzungstranche

Die nächste Codeänderung ist ausschließlich U7 einschließlich minimal
notwendiger U6-Schnittstellenkorrekturen. U1 bis U5 bleiben vorhandene,
validierte Basis und dürfen nur bei einem reproduzierten U7-blockierenden
Fehler geändert werden. Dieser Schnitt verhindert, dass der große Watch-, IPC-
und E2E-Bereich erneut ohne neuen Befund geöffnet wird.

### U1. Watch-Contract und CLI-Einstieg

**Ziel:** Den read-only AXI-Befehl mit verpflichtender Run-ID und valider
`--until`-Auswahl registrieren.

**Requirements:** R1, R2, R3, R7

**Dependencies:** Keine.

**Files:** `internal/cli/axi.go`, `internal/cli/axi_watch.go`,
`internal/cli/axi_watch_test.go`

**Approach:** `axi watch` als Read Surface registrieren. Nur `attention` und
`terminal` akzeptieren; unbekannte Werte liefern den bestehenden strukturierten
Usage-Fehler. Die Telemetrie bleibt auf Zustandsfingerprints begrenzt.

**Patterns to follow:** `internal/cli/axi_query.go`,
`internal/cli/telemetry.go`, `internal/cli/axi_drive.go`.

**Test scenarios:**

- Ohne `--run` entsteht ein strukturierter Usage-Fehler; kein Branch-Fallback.
- `attention` ist der Default, `terminal` wird akzeptiert und ein anderer Wert
  wird als Usage-Fehler abgewiesen.
- Der Befehl erzeugt keinen Pageview und trägt weder Run-ID noch Rohdaten in
  die Telemetrie.

**Verification:** CLI-Hilfe und Unit-Tests beweisen den öffentlichen
Flag-Contract (`unit_mocked`).

### U2. Autoritative ereignisgesteuerte Beobachtung

**Ziel:** Einen Run ohne Polling-Dauerschleife bis zum passenden Stoppgrund
beobachten.

**Requirements:** R2, R3, R5, R6

**Dependencies:** U1.

**Files:** `internal/cli/axi_watch.go`, `internal/cli/axi_watch_test.go`

**Approach:** Ohne `EnsureDaemon` zuerst den persistierten Run lesen, bei
terminalem Run direkt rendern und bei aktivem Run ausschließlich einen bereits
erreichbaren Daemon abonnieren. Vor und nach der Subscription sowie nach jedem
Event wird der autoritative Runzustand gelesen. Ein monotonicer Timer wird nach
jedem Read bis zur nächsten Quiet-Grenze gesetzt; sein Ablauf erzwingt genau
einen erneuten Read, statt eine Polling-Schleife zu starten. Bestehende
`runView`, `ciReadyToMerge` und Quiet-Semantik bestimmen den Stoppgrund. Ein
geschlossener Stream wird immer gegen einen letzten Read abgeglichen: terminal
wird ehrlich gerendert, sonst wird `stream-interrupted` gemeldet und der Run
unangetastet gelassen. Kann auch der finale Read nicht erfolgen, liefert AXI
den bestehenden strukturierten Betriebsfehler. Im Terminal-Modus latcht ein
festgestelltes Quiet bis zu einem neuen Stream-Ereignis; dieselbe alte
Aktivitätszeit darf keinen sofort erneut fälligen Timer erzeugen.

**Patterns to follow:** `internal/ipc/client.go`,
`internal/daemon/manager.go`, `internal/cli/axi_drive.go`.

**Test scenarios:**

- Bereits geparktes Gate endet in `attention` sofort ohne IPC-Mutation.
- Gate zwischen erster Aufnahme und Subscription wird durch die zweite
  Aufnahme erkannt.
- Ein aktivierter Stream führt bei Gate, CI-Bereitschaft und terminalem Zustand
  zum richtigen Stoppgrund.
- Keine Stream-Ereignisse bis zur Quiet-Deadline führen nach dem Timer-Read zu
  `attention: quiet`; frische Aktivität vor der Deadline verschiebt sie.
- Timer-Reset, deaktivierte Quiet-Schwelle und Race zwischen Timer und Event
  werden deterministisch abgedeckt.
- Ein bereits quietter, weiterhin aktiver Terminal-Run setzt ohne neues Event
  keinen weiteren Quiet-Read in Gang.
- `terminal` beobachtet Gate, Quiet und `checks-passed` ohne Zwischen-Output
  weiter; `attention` kehrt jeweils zurück.
- Geschlossener Stream mit weiterhin aktivem Run liefert `stream-interrupted`;
  geschlossener Stream mit terminalem Run liefert dessen Ergebnis; ein
  fehlgeschlagener finaler Read liefert den strukturierten Betriebsfehler.
- Ein nicht laufender oder nicht erreichbarer Daemon liefert einen read-only
  Fehler und startet weder Recovery noch Resume.
- Kontextabbruch, SIGINT und SIGTERM beenden nur die Subscription, geben den
  definierten `interrupted`-Snapshot aus und lösen keinen Cancel-IPC-Aufruf aus.

**Verification:** Deterministische CLI-/IPC-Tests (`integration_local`) zeigen
die Beobachtungs- und Nichtmutationsgrenze. Sie beweisen keine automatische
Codex-Fortsetzung.

### U2a. Unterbrechbare Subscription-Verbindung

**Ziel:** Schon der Handshake einer Watch-Subscription reagiert auf
Kontextabbruch und kann den Watch-Prozess nicht festhalten.

**Requirements:** R6

**Dependencies:** U1.

**Files:** `internal/ipc/client.go`, `internal/ipc/subscribe_test.go`,
`internal/cli/axi_watch.go`, `internal/cli/axi_watch_test.go`

**Approach:** Eine kontextfähige Subscription-Variante begrenzt den Handshake
und schließt ihre Verbindung bei Kontextende. `axi watch` leitet aus
SIGINT/SIGTERM einen eigenen Notify-Context ab, damit der laufende Pipeline-Run
nicht abgebrochen wird und der definierte `interrupted`-Snapshot entsteht.

**Patterns to follow:** `internal/daemon/daemon.go` für Signal-Kontexte,
`internal/ipc/client.go` für Connection-Cleanup.

**Test scenarios:**

- Ein Daemon, der den Subscribe-Handshake nicht beantwortet, wird durch
  Kontextabbruch beendet und hinterlässt keine blockierte Verbindung.
- SIGINT und SIGTERM während Handshake und Stream führen zu Exit 130 und keinem
  `CancelRun`-IPC-Aufruf.

**Verification:** IPC- und Subprocess-Harness liefern `integration_local`-
Evidenz für die lokale Unterbrechbarkeit; sie verändern keinen Pipeline-Run.

### U3. Begrenzte TOON-Rückgabe und Ausstiegssemantik

**Ziel:** Jeder Beobachtungsabschluss ist für Agenten eindeutig lesbar und
behält die etablierten AXI-Exitcodes bei.

**Requirements:** R4, R6, R7

**Dependencies:** U2, U2a.

**Files:** `internal/cli/axi_watch.go`, `internal/cli/axi_watch_test.go`

**Approach:** TOON enthält einen Watch-Block mit Stoppgrund, Terminalstatus,
`supervision: active_agent_required` und `auto_resumed: false`. Watch-Gates nutzen
einen eigenen begrenzten Renderer mit sichtbarer Gesamtzahl und
Trunkierungsmarker; gespeicherte Fehler und Zusammenfassungen sind ebenfalls
begrenzt. Terminale Ausgaben nutzen die bestehenden Outcome-Regeln. Gate, Quiet
und `checks-passed` sind beobachtete Ergebnisse mit Exitcode 0;
failed/cancelled und `stream-interrupted` bleiben nicht-null; SIGINT/SIGTERM
enden mit 130.

**Patterns to follow:** `internal/cli/axi_render.go`,
`internal/cli/axi_drive.go`.

**Test scenarios:**

- Gate enthält Watch-Grenze und bestehende Gate-Hilfe, aber keine automatische
  Antwortbehauptung.
- `checks-passed` bleibt von `passed` unterscheidbar.
- Failed und cancelled geben den gespeicherten Fehler samt terminalem
  Stoppgrund aus und enden nicht erfolgreich.
- Viele Findings und ein langer gespeicherter Fehler bleiben begrenzt und
  markieren ihre Trunkierung sichtbar.
- Rohinhalt der IPC-Events, Diffs und Logs erscheint nicht im TOON-Output.

**Verification:** Format- und Exitcode-Tests (`unit_mocked`) sichern den
maschinellen Contract; sie beweisen keine Scheduler-Integration.

### U4. Dokumentierte aktive Agentenrunde

**Ziel:** Installierte Agent-Skills und veröffentlichte Dokumentation erklären
den Watcher einheitlich als Read-only-Baustein des opt-in Codex-Supervisors.

**Requirements:** R8

**Dependencies:** U1, U2, U2a, U3.

**Files:** `internal/skill/skill.go`, `skills/no-mistakes/SKILL.md`,
`docs/src/content/docs/guides/agents.md`,
`docs/src/content/docs/reference/cli.md`,
`internal/cli/axi_guidance.go`, `internal/cli/axi_guidance_test.go`

**Approach:** Die generierte Skillquelle wird zuerst geändert, dann mit
`make skill` gerendert. Live-Hilfe und Agent-Guide enthalten dieselbe
Invariante: Ein vordergründiger Watch hilft nur innerhalb eines lebenden Turns;
für autonome CLI-Begleitung wird der opt-in Supervisor gerüstet und sein Hook
explizit installiert.

**Patterns to follow:** `AGENTS.md` Abschnitt „Agent-Guidance Surfaces“ und
`internal/cli/axi_guidance_test.go`.

**Test scenarios:**

- Die drei Guidance-Flächen trennen vordergründigen Watch und opt-in
  Supervisor, erklären dessen Hook-Grenze und behaupten keine Autonomie ohne
  installierten Hook.
- `make skill` erzeugt keinen Drift in `skills/no-mistakes/SKILL.md`.

**Verification:** Guidance-Synchronisation und generierter-Skill-Check
(`integration_local`) beweisen Dokumentationskonsistenz, nicht die
Scheduler-Ausführung.

### U5. End-to-End-Beobachtung am echten Daemon-Harness

**Ziel:** Die neue Oberfläche über den vorhandenen Daemon- und Subscription-
Pfad absichern.

**Requirements:** R1 bis R8

**Dependencies:** U1 bis U4 inklusive U2a.

**Files:** `internal/e2e/axi_journey_test.go` oder
`internal/daemon/subscribe_recover_test.go`

**Approach:** Den bestehenden Harness für einen Run mit Gate und einen
abgeschlossenen Run erweitern. Die Tests verwenden die reale lokale
IPC-Subscription und den Test-Daemon, aber keine reale Codex-Session.

**Test scenarios:**

- Ein Gate-Run liefert `attention` und bleibt geparkt, bis eine getrennte,
  explizite `axi respond`-Aktion folgt.
- Ein terminaler Run liefert sein finales Outcome.
- Ein Prozess-SIGINT und -SIGTERM liefert den definierten Exitcode, beendet nur
  die Beobachtung und lässt den Daemon-Run aktiv.
- Unterbrochene Beobachtung beeinflusst den Run nicht.

**Verification:** `make e2e` liefert `integration_local`-Evidenz für CLI,
Daemon und IPC. Es beweist nicht, dass Codex CLI oder ein ChatGPT-Scheduler
automatisch eine neue Runde startet.

### U6. Opt-in Supervisor-Registrierung und Codex-Stop-Hook

**Ziel:** Den aktiven Run sicher an einen späteren Stop-Hook derselben
Arbeitskopie binden, ohne Codex-Session-Dateien zu lesen oder globale Hooks zu
installieren.

**Requirements:** R9, R10, R13

**Dependencies:** U1 bis U3.

**Files:** `internal/cli/axi_supervise.go`,
`internal/cli/axi_supervise_test.go`, `internal/supervision/**`.

**Approach:** `axi supervise arm --run <id>` validiert den bekannten aktiven
Run und schreibt eine atomare, eng berechtigte Registrierung unter dem
No-Mistakes-State-Root. `axi codex-hook` liest genau ein Codex-Hook-JSON von
stdin, akzeptiert nur `Stop` samt nichtleerem `session_id` und gleichem `cwd`,
und übernimmt höchstens eine gerüstete Registrierung. Die CLI installiert oder
ändert keinen Codex-Hook; die Dokumentation zeigt die manuelle,
vertrauenspflichtige Hook-Definition.

**Betriebsgrenze:** Smart Commit darf pro Arbeitskopie nur einen betreuten
Codex-Run gleichzeitig führen. Da `arm` keine Codex-Session-ID erhält, kann
der erste Stop-Hook diese Grenze nicht kryptografisch beweisen. `arm` gibt sie
deshalb sichtbar aus; eine parallele Session im selben Worktree ist ein
nicht unterstützter Ablauf und kein Anlass für eine zweite Owner-Registry.

**Test scenarios:**

- Arm ohne explizite Run-ID oder mit terminalem/fremdem Run schlägt ohne
  Registrierung fehl.
- Ein fremdes Hook-Event, fehlende Session-ID oder anderer `cwd` kann keine
  Registrierung übernehmen.
- Gleichzeitige Stop-Hooks können nur einmal claimen.
- Eine vorhandene `awaiting_user`-Registrierung kann weder von einer fremden
  Session noch ohne Simons Antwort reaktiviert werden; eine terminale
  Registrierung wird nie neu aktiviert.
- `arm` macht die Einzel-Session-pro-Arbeitskopie-Grenze sichtbar; die
  Dokumentation behauptet keine technisch erzwungene Cross-Session-Isolation.
- Fehlende `turn_id`, fremde `repo_id`, wiederverwendeter `cwd` und eine
  abweichende Run-Repositorybindung geben kein Fortsetzungssignal aus und
  pausieren nur mit einer begrenzten Diagnose.

**Verification:** Zustands- und Hook-Parser-Tests (`unit_mocked`) belegen die
Zuordnung und den Opt-in-Contract.

### U7. Hook-native Fortsetzung, Heartbeat und Nutzer-Handoff

**Ziel:** Nach einem regulären Codex-Turn-Ende dieselbe sichtbare Sitzung bei
einem relevanten AXI-Ereignis oder Heartbeat fortsetzen und bei CEO-Gates
sicher an Simon übergeben.

**Requirements:** R10, R11, R12, R14, R15, R16, R17

**Dependencies:** U2, U3, U6.

**Files:** `internal/supervision/store.go`, `internal/supervision/store_test.go`,
`internal/cli/axi_supervise.go`, `internal/cli/axi_supervise_test.go`,
`internal/cli/axi_watch.go`, `internal/cli/axi_watch_test.go`,
`internal/config/config.go`, `docs/src/content/docs/reference/global-config.md`,
`docs/src/content/docs/reference/cli.md`, `docs/src/content/docs/guides/agents.md`,
`internal/skill/skill.go`.

**Verbindliches Vor-Gate:** Vor jeder U7-Codeänderung wird ein von Simon
freigegebener, temporärer `live_local`-Probe mit einem statischen Test-Hook
durchgeführt. Er liefert genau zwei unterscheidbare, aufeinanderfolgende
  `decision: block`-Signale und endet danach wirklich. Der Probe dokumentiert
  nur datensparsame Strukturwerte nach R16. Der nichtinteraktive Codex-CLI-
  Probe vom 2026-07-16 hat die technische Folgefortsetzung und die
  Deduplizierungsbeziehung belegt; die sichtbare TUI-Session bleibt als
  Abschlussbeweis erforderlich. Scheitert diese sichtbare Fortsetzung oder
  zeigt die reale Payload keinen robusten Deduplizierungspfad, wird U7 nicht
  implementiert; der Plan kehrt zur Architekturentscheidung zurück.

**Approach:** Der Stop-Hook verwendet `axi watch --until attention` als
einzigen Event-Sensor und übersetzt dessen begrenzten, autoritativen Snapshot
in das dokumentierte Stop-Hook-JSON mit `decision: block` und begrenztem,
nichtleerem `reason`. Dieser Grund ist ein statischer Allowlist-Code wie
`nm_event=technical_gate`, `nm_event=heartbeat`, `nm_event=checks_passed`,
`nm_event=terminal`, `nm_event=stale` oder `nm_event=watch_fault`; Details
liest die fortgesetzte Sitzung erst selbst über den gebundenen Statuspfad.
Vor dem Signal persistiert der Hook Ereignisfingerprint, `turn_id` und
Handoff-Zustand atomar. Ein `ask-user`-Gate wird einmal sichtbar gemacht und
danach als `awaiting_user` geparkt; technische Findings werden einmal an
denselben Codex-Turn übergeben, `checks-passed` wird als eigener Smart-Commit-
Handoff behandelt.

Im Supervisor-Modus besitzt ausschließlich `next_heartbeat_at` die
Zeitsteuerung: Der Hook nutzt eine interne Watch-Variante, deren öffentliche
Quiet-Deadline dort keine eigenständige Ausgabe auslöst. Stream-Ereignisse
wecken sofort; ohne Ereignis endet genau die Heartbeat-Frist. Damit gibt es
keinen konkurrierenden Quiet- und Heartbeat-Timer.

Eine zentrale, getestete Klassifikation ordnet jeden autoritativen Snapshot
als `ask_user`, `technical_gate`, `checks_passed`, `terminal`, `stale` oder
`watch_fault` ein. Sie wertet die geparsten Gate-Aktionen aus, nicht nur
`AwaitingAgentSince`; unbekannte oder ungültige Gate-Aktionen sind
fail-closed `ask_user`. Die Heartbeat-Frist bleibt fest bei fünf Minuten.
Nur `supervision_max_stale_heartbeats` ist konfigurierbar, akzeptiert 1 bis 6
und fällt bei ungültigen Werten auf 4 zurück. Nach dem Budget geht der Zustand
mit einem einmaligen Stale-Handoff auf Pause. `checks-passed` geht nach seinem
einmaligen Merge-Handoff auf `awaiting_merge_result`; Watcher-Fehler gehen nach
höchstens einem technischen Handoff ebenfalls auf Pause. Die manuelle
Hook-Anleitung verlangt einen Timeout oberhalb der Frist mit Reserve.
`codex exec resume`, ein detached Worker und dessen Prozess-/Lock-Lebenszyklus
werden entfernt. Eine lokale Benachrichtigung bleibt optional und darf den
sichtbaren Hook-Handoff nicht ersetzen.

| Klassifikation | Einmalige sichtbare Aktion | Folgezustand |
| --- | --- | --- |
| `ask_user` | Kein `decision: block`; Codex endet für Simon | `awaiting_user` |
| `technical_gate` | Statischer technischer Ereigniscode | laufende Session kann antworten; nächster Stop beobachtet erneut |
| `checks_passed` | Statischer Merge-Ereigniscode | `awaiting_merge_result`, keine automatische Wiederholung |
| `terminal` | Statischer Abschlusscode | `completed` |
| `stale` | Statischer Stale-Ereigniscode | `paused`, bewusste neue Aktion nötig |
| `watch_fault` | Statischer Fehler-Ereigniscode | `paused`, kein Retry-Loop |

**Manueller Operator-Preflight:** Vor der sichtbaren TUI-Abnahme prüft der
Nutzer den ausdrücklich installierten Stop-Hook-Befehl und seinen Hash sowie
`timeout >= 360` Sekunden (fünf Minuten Heartbeat plus mindestens 60 Sekunden
Reserve). Das Receipt speichert nur Codex-Version, Hash, Timeout und
Evidenzklasse. Ohne diesen manuellen Preflight darf die Abschlussmeldung nur
`local_hook_harness_only` behaupten.

**Test scenarios:**

- Ein Ereignis liefert genau ein gültiges Hook-Fortsetzungssignal, auch wenn
  Hook und Stream mehrfach eintreffen.
- Ein Signal enthält ausschließlich das dokumentierte Stop-Hook-JSON mit
  begrenztem `reason`; eine Fixture-Regression schützt vor leerem Grund,
  Klartext-Ausgabe, ungebundenen Run-/Session-Daten oder untrusted Finding-,
  Fehler- und PR-Text.
- Ein aktiver Run beginnt erst beim folgenden Stop eine neue Watch-Phase und
  wartet dann bis zu einem neuen Event oder der frisch geplanten Heartbeat-Frist.
- Ein geparktes `ask-user`-Gate geht nach genau einem Handoff auf
  `awaiting_user`, startet vor Simons Antwort kein weiteres Hook-Signal und
  hängt sich erst nach einem späteren Ende derselben Session wieder an.
- Ein technisches Gate und `checks-passed` werden nicht als CEO-Gate markiert;
  beide erzeugen je einen klaren technischen beziehungsweise Merge-Handoff.
- Unbekannte oder ungültige Gate-Aktionen bleiben fail-closed beim
  `ask_user`-Pfad; die Klassifikation hängt nicht allein an
  `AwaitingAgentSince`.
- Ein alter Quiet-Zustand erzeugt nach sichtbarem Heartbeat keinen unmittelbaren
  zweiten Handoff; ein Heartbeat erscheint höchstens einmal pro Frist.
- Vier Heartbeats ohne autoritativen Fortschritt führen nach der nächsten
  Frist zu genau einem Stale-Handoff und einer Pause; ein echter Fortschritt
  setzt das Budget zurück. Die Werte 1 und 6 werden akzeptiert, ungültige
  Werte fallen deterministisch auf 4 zurück.
- Derselbe `turn_id` oder unveränderte Ereignisfingerprint erzeugt kein
  zweites Signal, auch wenn Codex den Stop-Hook nach einer automatischen
  Fortsetzung erneut aufruft.
- Die Standard-Heartbeat-Frist liegt unter der dokumentierten Hook-Timeout-
  Reserve; ein Testlauf über mindestens eine Frist beweist, dass der Hook vor
  dem nächsten sichtbaren Handoff nicht ausläuft.
- Terminales Ergebnis erzeugt einen finalen sichtbaren Handoff, markiert danach
  die Registrierung als abgeschlossen und kann keinen alten Run wiederbeleben.
- `checks-passed` erzeugt genau einen Merge-Handoff und geht auf
  `awaiting_merge_result`; ein Watcher-Transport-, Parse- oder Betriebsfehler
  erzeugt höchstens einen technischen Handoff und geht dann auf Pause.
- Die Supervisor-Watch hat genau einen Heartbeat-Timer; ein Quiet-Signal des
  öffentlichen Watch-Contracts kann im Supervisor-Modus keinen zweiten
  Handoff erzeugen.
- Der Operator-Preflight prüft den Hook-Timeout, den installierten Befehl und
  dessen Hash; ohne ihn bleibt der sichtbare TUI-/Chat-Claim gesperrt.

**Verification:** Unit- und lokaler Hook-Harness (`integration_local`) belegen
Deduplication, Signalform, Heartbeat-Rearming und CEO-Stopp. Ein echter,
opt-in Codex-CLI-Run mit Hook liefert zusätzlich `live_local`-Evidenz dafür,
dass derselbe Chat sichtbar fortgesetzt wird.

## Fallmatrix und Qualitätsgates

| Fall | Erwartung | Evidenzklasse | Gate |
| --- | --- | --- | --- |
| Bereits geparktes Gate | `attention: gate`, keine Mutation | integration_local | erforderlich |
| Gate-Race beim Start | Zweite Aufnahme erkennt das Gate | integration_local | erforderlich |
| CI grün, PR offen | `checks-passed`, nicht `passed` | integration_local | erforderlich |
| Quiet | Hinweis, keine Reparatur oder Abbruchaktion | unit_mocked | erforderlich |
| Verlorenes Event bei offenem Stream | Quiet-Deadline löst autoritativen Read aus, kein endloses Warten | integration_local | erforderlich |
| Bereits quietter Terminal-Run | Quiet-Latch verhindert hektische Reads bis zu einem neuen Event | integration_local | erforderlich |
| Erfolgreich terminal | `passed`, Exit 0 | integration_local | erforderlich |
| Failed/cancelled | Persistierter Fehler, nicht-null Exit | integration_local | erforderlich |
| Stream-Abbruch | Letzter Read oder `stream-interrupted` Exit 1; bei fehlendem Read strukturierter Fehler | integration_local | erforderlich |
| SIGINT/SIGTERM | `interrupted`, Exit 130, Subscription endet, Pipeline läuft weiter | integration_local | erforderlich |
| Hängender Subscribe-Handshake | Kontext löst die Verbindung, kein blockierter Watch-Prozess | integration_local | erforderlich |
| Daemon nicht erreichbar | Strukturierter Fehler ohne Start, Recovery oder Resume | integration_local | erforderlich |
| Fremder oder paralleler Stop-Hook | Keine Übernahme der Registrierung | unit_mocked | erforderlich |
| Zwei Codex-Sessions im selben Worktree | Nicht unterstützter Betriebsmodus; `arm` benennt die Einzel-Session-Grenze sichtbar | unit_mocked | erforderlich |
| Doppelte Ereignisse | Genau ein Hook-Fortsetzungssignal pro Ereignisfingerprint | integration_local | erforderlich |
| Heartbeat-Rearming | Kein unmittelbarer zweiter Handoff aus demselben Quiet-Zustand | integration_local | erforderlich |
| Stop-Hook-Rekursion | Derselbe `(turn_id, Ereignisfingerprint)` erzeugt kein zweites Signal; `stop_hook_active` allein sperrt keinen neuen legitimen Turn | integration_local | erforderlich |
| Technisches Gate | Einmaliger technischer Handoff, nicht `awaiting_user` | integration_local | erforderlich |
| CEO-Gate nach Handoff | `awaiting_user`, keine Fortsetzung vor Simons Antwort; dieselbe Session kann sich danach wieder anhängen | integration_local | erforderlich |
| Checks grün | Eigener Merge-/Handoff-Zustand, nicht `awaiting_user` | integration_local | erforderlich |
| Checks grün nach Handoff | `awaiting_merge_result`, keine wiederholte Supervision ohne bewusste neue Aktion | integration_local | erforderlich |
| Vier Heartbeats ohne Fortschritt | Ein Stale-Handoff, anschließend Pause; echter Fortschritt setzt das Budget zurück | integration_local | erforderlich |
| Watcher-Betriebsfehler | Höchstens ein technischer Handoff, anschließend Pause statt Retry-Schleife | integration_local | erforderlich |
| Hook-Timeout | Heartbeat liegt unter Timeout mit Reserve; kein stiller Autonomieanspruch nach Timeout | live_local | erforderlich |
| Zwei Hook-Fortsetzungen | Der temporäre Probe liefert zwei unterscheidbare Fortsetzungen derselben sichtbaren Sitzung und endet danach | live_local | Vor-Gate für U7 |
| Hook-Input und Reason-Grenze | Fehlender Turn, fremdes Repository oder untrusted Finding-/Fehlertext erzeugt keinen unsicheren Handoff | unit_mocked | erforderlich |
| Supervisor-Zeitgeber | Nur die Heartbeat-Frist löst ohne Stream-Ereignis einen Handoff aus | integration_local | erforderlich |
| Operator-Preflight | Installierter Hook, Hash und Timeout von mindestens 360 Sekunden sind datensparsam quittiert | live_local | Abschlussgate |
| Echte Codex-Fortsetzung | Dieselbe sichtbare Sitzung wird nach AXI-Ereignis fortgesetzt | live_local | erforderlich |

## Risiko und Begrenzung

- Ein Stream kann Ereignisse verlieren oder ohne terminales Ereignis enden.
  Die Quiet-Deadline verhindert endloses Warten bei offenem Stream; der finale
  autoritative Read verhindert eine falsche Erfolgsbehauptung bei Stream-Ende.
- `--until terminal` kann bei einem Gate absichtlich lange warten. Seine Hilfe
  muss deshalb die externe Supervisor-Pflicht ausdrücklich nennen.
- Der Codex-Hook ist ein lokaler Vertrauensanker. Er darf erst nach sichtbarer
  Installation und Hash-Prüfung laufen und muss sich bei unbekanntem Input
  wirkungslos verhalten.
- Die erste Version bindet einen gerüsteten Run nicht technisch an die
  ursprünglich armende Codex-Session: Der Hook erhält diese ID erst beim
  Turn-Ende. Deshalb gilt sichtbar die Betriebsgrenze „eine Codex-Session je
  Arbeitskopie“. Wer parallel in derselben Arbeitskopie arbeiten muss, braucht
  einen späteren Owner-Handshake statt stiller Nebenläufigkeit.
- Ein Stop-Hook kann nur innerhalb seiner konfigurierten Laufzeit warten. Der
  echte lokale Test muss deshalb den gewählten Hook-Timeout und einen Run über
  mindestens eine Heartbeat-Frist belegen. Die dokumentierte Konfiguration
  setzt den Timeout über fünf Minuten plus Reserve; ohne diese sichtbare
  Konfiguration wird keine Dauerbetreuung behauptet.
- Ein aktiver, aber festgefahrener Run darf keine unbegrenzten Codex-Turns
  verbrauchen: Nach der konfigurierbaren Zahl sichtbarer Heartbeats ohne
  autoritativen Fortschritt pausiert die Supervision. Der Standard 4 ist
  bewusst großzügig, aber endlich.
- Der lokale State ist eine Same-User-Vertrauensgrenze, kein signierter
  Herkunftsnachweis eines Hook-Events. Die Version schützt vor unbeabsichtigter
  Cross-Worktree-Zuordnung und Leak in den Codex-Grund, nicht vor bösartigen
  Prozessen mit demselben Betriebssystemkonto.

## Quellen und Forschung

- `internal/ipc/protocol.go`, `internal/ipc/client.go`,
  `internal/daemon/daemon.go`: vorhandener run-scoped Subscription-Contract.
- `internal/cli/axi_drive.go`, `internal/cli/axi_query.go`:
  bestehende Gate-, CI- und Outcome-Semantik.
- `internal/daemon/manager.go`: Subscriptions haben keine Startaufnahme und
  können bei langsamen Empfängern Ereignisse verlieren.
- [No-Mistakes CLI-Referenz](https://kunchenguid.github.io/no-mistakes/reference/cli/).
- [OpenAI Codex Hooks](https://learn.chatgpt.com/docs/hooks): `Stop` ist
  turn-scoped, liefert `session_id` und `cwd` und kann per `decision: block`
  dieselbe Sitzung fortsetzen.

## Definition of Done

- `axi watch --run <id>` erfüllt R1 bis R8 und hat keinen mutierenden
  Nebeneffekt.
- Der opt-in Supervisor erfüllt R9 bis R17: pro Run nur Hook-native
  Fortsetzungen, keine fremde Session, kein wiederholtes CEO-Gate ohne neue
  Nutzerantwort und keine Wiederholungs-Schleife aus einem alten
  Quiet-Zustand. Technische und Merge-Handoffs bleiben dabei ausdrücklich
  zulässig.
- Unit-, Race-/Stream- und lokale Daemon-E2E-Fallmatrix ist grün.
- Skill, Live-Hilfe und Dokumentation sind synchron generiert.
- `make lint`, `go test -race ./...`, `make e2e` und
  `go build -o ./bin/no-mistakes ./cmd/no-mistakes` bestehen.
- Die Abschlussmeldung benennt die Evidenzklasse und trennt lokale Harness-
  Evidenz von einer echten Codex-CLI-Hook-Fortsetzung.

## Implementierungsstand

- **U1 bis U3 sowie der Watch-Teil von U4:** lokal umgesetzt und durch
  fokussierte CLI-, IPC- und Renderer-Tests sowie `make skill` geprüft. Die
  derzeit gerenderte Supervisor-Anleitung beschreibt noch den verworfenen
  Worker-/Resume-Pfad und ist Bestandteil der U7-Ersetzung, nicht Evidenz für
  die Hook-native Architektur.
- **U5:** Der bestehende lokale E2E-Harness wurde ausgeführt. Sein erster Lauf
  zeigte einen zeitabhängigen Zwischenstands-Flake außerhalb dieses Changes;
  die gezielte Wiederholung `TestUserJourney/claude` war grün. Der Harness
  beweist noch keinen `axi watch`-spezifischen Daemonfall.
- **U6:** lokal umgesetzt: run- und arbeitskopiegebundene Registrierung sowie
  Stop-Hook-Parser. Die bestehenden Parser-Tests bleiben Ausgangspunkt.
- **U7:** durch die Entscheidung vom 2026-07-16 neu zu implementieren. Der
  bisherige Worker-/`codex exec resume`-Pfad ist ausdrücklich verworfen;
  erforderlich sind Hook-native Fortsetzung, Heartbeat-Rearming und ein
  echter sichtbarer Session-Test. **Vorbedingung:** Der technische R16-
  Zwei-Handoff-Probe ist erfolgreich; der sichtbare TUI-Nachweis bleibt
  Abschlussgate.
- **Offen für `live_local`:** Ein echter, von Simon überprüfter Eintrag in
  `~/.codex/hooks.json` und ein kurzlebiger Codex-CLI-Run müssen den Stop-Hook,
  mindestens einen Heartbeat und dieselbe sichtbare Sitzung beweisen. Diese
  persönliche Konfigurationsänderung wird weder automatisch installiert noch
  als bereits geprüft behauptet.

## Entscheidungs- und Annahmenledger

- **Entscheidung:** `attention` ist der Default, `terminal` ist passiv.
  **Quelle:** bestehende Gate-Semantik und User-Ziel. **Status:** entschieden;
  verhindert einen irreführenden Autonomie-Contract.
- **Entscheidung:** Der externe Teil ist ein expliziter Codex-Stop-Hook mit
  Hook-nativer Fortsetzung, keine heimliche Cron-Aufgabe und kein zweiter
  Codex-Prozess. **Status:** entschieden; Nutzerentscheidung Option 1 am
  2026-07-16, nach Revalidierung gegen Codex 0.144.5 und die aktuelle
  Hook-Dokumentation.
- **Entscheidung:** Kein neuer Daemon- oder Datenbankvertrag. **Status:**
  entschieden; der vorhandene Eventstream deckt den Bedarf.
- **Entscheidung:** Die erste Hook-native Version unterstützt nur eine
  betreute Codex-Session je Arbeitskopie. **Status:** entschieden am
  2026-07-16; sichtbar dokumentierte Betriebsgrenze statt zusätzlicher
  SessionStart-/Owner-Handshake-Schicht.
- **Entscheidung:** Heartbeats bleiben auf fünf Minuten fest; ihre maximale
  Zahl ohne autoritativen Fortschritt wird über
  `supervision_max_stale_heartbeats` von 1 bis 6 gesteuert, Standard und
  Fallback sind 4. **Status:** entschieden am 2026-07-16; danach einmaliger
  Stale-Handoff und Pause statt Endlosschleife.
- **Entscheidung:** Die sichtbare TUI-Abnahme hat einen manuellen
  Operator-Preflight für Stop-Hook-Befehl, Hash und Timeout von mindestens
  360 Sekunden. **Status:** Review-Fix am 2026-07-16; keine stille Änderung
  persönlicher Codex-Konfiguration.
- **Befund:** Codex Stop-Hooks stellen aktuell einen nativen, turn-scoped
  Fortsetzungsvertrag bereit; sie können dieselbe Sitzung mit einem offiziellen
  Signal weiterführen. **Status:** durch aktuelle Codex-Dokumentation und
  lokale CLI-Hilfe belegt; der reale sichtbare Chat-Lauf bleibt ein Pflichttest.

## Elons Principles Guard

Anforderung geprüft: Ein Watcher allein ist nicht genug; ein minimaler,
opt-in Stop-Hook-Adapter ist notwendig. Gelöscht: detached Worker,
`codex exec resume` und dessen zusätzlicher Prozess-/Lock-Lebenszyklus.
Vermieden: Cron/Autostart, eine zweite Polling-Implementierung,
Session-Datei-Raten und eine globale Hook-Installation. Vereinfacht vor
Automatisierung: vorhandenen IPC-Stream als Wecksignal nutzen, Codex-
Session-ID ausschließlich vom offiziellen Hook erhalten. Automatisierung endet
sichtbar an CEO-Gates.
