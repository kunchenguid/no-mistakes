---
title: Scope the Document step
description: Tell Document which files it may edit and see where it made changes.
---

Put any file restriction in the run's intent or captured caller restrictions so the Document agent receives it. For example, this run limits edits to the settings file.

```sh
no-mistakes axi run --intent "Change the default in config/settings.yaml. Edit only config/settings.yaml."
```

Document sees the file list from the submitted change on its first pass and on fix rounds. If the restriction rules out a repair, the agent is instructed to leave the file alone and report an `ask-user` finding with the file and proposed repair. A recorded human decision can amend the original intent.

On an unrestricted run, Document can update an owner document outside the submitted change. The Document gate and `no-mistakes axi status` show those paths, and the PR's Pipeline section lists them under Document. The list reports the edits for inspection.
