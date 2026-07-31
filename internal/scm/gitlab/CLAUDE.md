# GitLab Backend (`internal/scm/gitlab`)

- The backend is pinned against `glab v1.5x`, whose flag surface drifts between versions: the auth check must be host-scoped (`--hostname <host>`, falling back to unscoped only when the host is unknown), `glab mr list` no longer accepts `--state opened`, and the daemon's detached-HEAD worktree breaks `glab ci get`, so pipeline jobs are read via the branch-independent `glab api .../pipelines/<id>/jobs` REST endpoint.
- The comments in `internal/scm/gitlab/gitlab.go` own the full rationale for each trap; extend them there when you hit new glab version drift.
