# Cluster namespace `no-mistakes`

Runs the GitHub publish-firewall check so developer workstations do not
install no-mistakes. The Mac `~/.no-mistakes` pipeline daemon stays on the
operator workstation and is not relocated here.

## LAN vs public

Self-hosted GitHub Actions runners **pull jobs outbound** to GitHub. GitHub
does not need to reach this cluster. There is no Ingress, public VIP, or
webhook for the firewall.

The portal Service is ClusterIP. Operators on the LAN (or in-cluster)
open the URL printed in the generic GitHub check. Match details never leave
the LAN.

If a future design required GitHub to push events inbound, that would be an
explicit public-VIP webhook. It is not required for required checks.

## Apply

```sh
kubectl apply -f deploy/no-mistakes/namespace.yaml
kubectl apply -f deploy/no-mistakes/config.yaml
kubectl apply -f deploy/no-mistakes/portal.yaml
kubectl apply -f deploy/no-mistakes/runner.yaml
```

`runner.yaml` is an Actions Runner Controller `AutoscalingRunnerSet`, the
runner mechanism this cluster already runs. Two operator-owned prerequisites
must exist first, or the check fails closed on every pull request:

1. The runner image must contain the `no-mistakes` binary on PATH. `check.sh`
   aborts with `conclusion=error` when `command -v no-mistakes` fails, so a
   stock `ghcr.io/actions/actions-runner` makes `publish-policy` a permanently
   red required check. Build the image from a pinned commit of this fork and
   push it to `registry.carverauto.dev`.
2. ARC must be installed in the cluster, and the `github-runner` secret must
   carry a GitHub App or PAT:

   ```sh
   kubectl -n no-mistakes create secret generic github-runner \
     --from-literal=github_token=<pat>
   ```

The ingest token is **not** an environment variable on the runner pod. A
`pull_request` workflow runs YAML from the PR head, so a pod-wide secret would
be readable by any contributor whose run is approved. Store it as a repository
secret and pass it to the action's `portal-token` input; GitHub withholds
repository secrets from fork pull requests, so ingest fails closed there
instead of handing out the secret. Only the portal pod reads
`firewall-ingest`.

`portal.yaml` claims a `ReadWriteOnce` PersistentVolumeClaim for
`/var/lib/no-mistakes`. The verdict store is the only place match details
exist, and the portal URL printed in the public GitHub check must still
resolve after a restart or rollout, so it must not be ephemeral. Set
`storageClassName` if the cluster has no default class; the Deployment uses
the `Recreate` strategy because the claim is single-writer.

Configured public product repositories are listed in the ConfigMap
(`carverauto/serviceradar` first; add SDK and others there). Each repository
still needs a ruleset requiring the `publish-policy` check pinned at a
commit SHA of this fork.
