---
title: Scope the Document step
description: Tell Document which files it may edit and see which paths it committed.
---

Put file restrictions in the run's [explicit intent](/no-mistakes/reference/cli/#intent-input) or [captured caller restrictions](/no-mistakes/reference/cli/#caller-restrictions). This example tells the agent to edit only the settings file.

```sh
no-mistakes axi run --intent "Change the default in config/settings.yaml. Edit only config/settings.yaml."
```

Document receives the run's intent and submitted paths on its first pass and on fix rounds, including when it also handles Lint. With a recorded submitted head and a base commit, Document diffs the submitted head against its merge base with the base commit resolved for the pass.

The submitted paths give the agent context for applying explicit file restrictions. If a restriction rules out a repair, the prompt tells the agent to leave the file untouched and return an `ask-user` finding naming the file and the proposed repair. A recorded human decision can amend the original intent.

On an unrestricted run, Document can update an owner document outside the submitted change. The report lists paths outside the submission that changed between the commits before and after the pass, including changes left by earlier steps if Document commits them. The AXI gate and `no-mistakes axi status` show a bounded preview. The TUI shows it next to the Document step when space permits. Run `no-mistakes axi logs --step document --full` for the complete recorded list. Quoted paths use Go string escapes.

AXI lists the preview in `document_off_change_paths` and adds `document_off_change_note` if paths were omitted. When a retry carries a Document result with reported paths, `axi logs --step document` reads the original step log and names that run in `source_run`.

The PR's Pipeline section can show up to five paths in sorted order, subject to the usual description size limits. [`pr.appendix: minimal`](/no-mistakes/reference/repo-config/#prappendix) omits this detail.
