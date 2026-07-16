# Claude-Stop-Hook-Parität – Arbeitsnotizen

Status: Arbeitsnotizen, nicht der finale Produktvertrag.

## Problemrahmen

Simon möchte, dass ein über Claude gestarteter No-Mistakes-Lauf dieselbe betreute Kette wie Codex erhält: kein manuelles Statusfragen, technische Ereignisse laufen weiter, echte Nutzerentscheidungen pausieren sichtbar.

## Gesicherte Fakten

- Claude Code 2.1.198 ist lokal installiert und besitzt einen offiziellen `Stop`-Hook mit `decision: "block"` und `reason`.
- Das Stop-Ereignis liefert `session_id`, `cwd`, `transcript_path`, `stop_hook_active` und die letzte Assistentenantwort, aber keinen dokumentierten `turn_id`.
- Die bestehende AXI-Supervision ist derzeit ausdrücklich Codex-spezifisch; sie bindet einen gerüsteten Run an Arbeitskopie, Repository, Branch und Codex-Session und begrenzt Heartbeats auf vier ohne Fortschritt.
- In `~/.claude/settings.json` existiert bereits ein globaler Stop-Notification-Hook. Ein No-Mistakes-Hook muss ihn ergänzen, nicht ersetzen.

## Zielvertrag in Arbeit

- Gleiche sichtbare Betreuung für Claude und Codex, soweit die jeweiligen offiziellen Hook-Verträge dies zulassen.
- Nur ein ausdrücklich gerüsteter Run im passenden Repository, Branch und Arbeitsverzeichnis darf beansprucht werden.
- Keine Run-, Log-, Finding-, Token- oder Sitzungsdaten im Hook-Output.
- Technische Ereignisse dürfen fortsetzen; `ask-user`, Fehler, Stale-Budget und Abschluss müssen eindeutig und endlich behandelt werden.
- Installation bleibt ein bewusstes Opt-in und muss eine vorhandene Claude-Hook-Konfiguration erhalten.

## Offene technische Frage

- Claude liefert keinen `turn_id` und dokumentiert eine Stop-Hook-Blockgrenze. Vor einer Paritätszusage ist ein kurzlebiger `live_local`-Probe mit zwei aufeinanderfolgenden Fortsetzungen derselben Claude-Session nötig; der Probe darf keine Pipeline mutieren und speichert keine Chatinhalte.

## Annahmen- und Entscheidungsledger

| ID | Klasse | Aktueller Stand | Vor |
| --- | --- | --- | --- |
| A1 | repo_evidence_pending | Ob Claudes Stop-Hook bei zwei echten Fortsetzungen trotz `stop_hook_active` die gleiche Session zuverlässig weiterführt, ist noch live zu prüfen. | Implementierungsfreigabe |
| D1 | user_decision | Das Ziel ist funktionale Parität, nicht bloß ein Claude-Skill-Text. | Produktvertrag |
| A2 | carry_visible | Claude braucht wahrscheinlich eine eigene Hook-Adapter-Route; der gemeinsame AXI-State sollte erhalten bleiben. | Planung |

## Nächster Punkt

Lokales Supervisor-Design und der Claude-Lifecycle-Probe gegen die offizielle Hook-Schnittstelle zusammenführen; erst danach den implementation-ready Schnitt festlegen.
