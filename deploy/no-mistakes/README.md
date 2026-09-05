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

Fill `github-runner` and `firewall-ingest` secrets before the runner
Deployment will start. Image tags are operator-owned.

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
