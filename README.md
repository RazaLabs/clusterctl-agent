# clusterctl agent

Sends alerts about your Kubernetes cluster to **clusterctl** on your phone, even when the app is closed.

It watches pods, workloads, nodes, volume claims and Gateway API resources with **read-only** access, using the same
checks as the app:

- a crash-looping pod, or one that can't pull its image or ran out of memory,
- a deployment, statefulset or daemonset with too few ready replicas,
- a failed job, a node that went NotReady, a lost volume claim,
- a Gateway that isn't programmed, an HTTPRoute that was rejected.

A problem must last 30 seconds (5 minutes for warnings like a stuck rollout) before it alerts, so blips and normal
rollouts stay quiet. It can also forward your **Alertmanager** alerts.

## How alerts reach your phone

```
clusterctl-agent (your cluster) ──HTTPS──▶ clusterctl relay ──▶ Apple Push Notification service ──▶ your phone
```

- The agent only makes outgoing connections, so it works behind firewalls and on private clusters.
- Alert details are encrypted (AES-256-GCM) with a key that stays in your cluster and on paired phones. The relay
  forwards sealed messages it can't read.
- Set `details: "false"` in the `clusterctl-agent-config` ConfigMap and the visible notification text becomes generic
  too: "Critical problem. Open clusterctl to see it."
- The relay knows a cluster only by a hash of its relay key.

## Install

### From the app

Settings › Alerts › **Install agent**. clusterctl first checks, permission by permission, that your account may install
it. You need rights to create a namespace, a ClusterRole and a ClusterRoleBinding, which usually means cluster-admin.
If you can't, it offers this YAML to send to an admin.

### Yourself

```sh
kubectl apply -f https://github.com/RazaLabs/clusterctl-agent/releases/latest/download/clusterctl-agent.yaml
kubectl -n clusterctl-system set env deploy/clusterctl-agent CLUSTER_NAME=prod-eu   # the name shown in notifications
```

On first start the agent creates its keys in the `clusterctl-agent` Secret. Then pair phones:

- Anyone whose account can read secrets in `clusterctl-system` just turns on **Notify this phone** in the app.
- Everyone else enters the pairing code:

  ```sh
  kubectl -n clusterctl-system get secret clusterctl-agent -o jsonpath='{.data.pairing-code}' | base64 -d
  ```

  Treat it like a password: it lets a phone receive and decrypt this cluster's alerts.

With GitOps (Argo CD, Flux), commit `deploy/clusterctl-agent.yaml` to your repo instead of installing from the app.

## What it creates

| Object | Purpose |
| --- | --- |
| Namespace `clusterctl-system` | Everything below lives here |
| ServiceAccount, Deployment, Service `clusterctl-agent` | The agent (10m CPU, 48Mi memory, non-root, read-only filesystem) |
| ClusterRole + Binding `clusterctl-agent` | get/list/watch pods, nodes, PVCs, namespaces, deployments, statefulsets, daemonsets, jobs, gateways, httproutes |
| Role + Binding `clusterctl-agent` | Its own Secret and ConfigMaps only |
| Secret `clusterctl-agent` | Relay key, encryption key, pairing code |
| ConfigMap `clusterctl-agent-config` | `details: "true"` or `"false"` |
| ConfigMap `clusterctl-agent-state` | Current problems and a heartbeat, which the app reads |

## Configuration

| Variable | |
| --- | --- |
| `RELAY_URL` | Where alerts go (the manifest sets clusterctl's relay) |
| `CLUSTER_NAME` | The name shown in notifications |
| `RELAY_CA` | A PEM file of extra CAs to trust, for a self-hosted relay with a private CA |
| `PORT` | HTTP port for `/healthz` and `/alertmanager` (8080) |
| `POD_NAMESPACE` | Where its Secret and ConfigMaps live (`clusterctl-system`) |
| `KUBECONFIG`, `KUBE_CONTEXT` | Run outside a cluster, for development |

## Alertmanager

Add a receiver, and keep your existing ones with `continue: true`:

```yaml
receivers:
  - name: clusterctl
    webhook_configs:
      - url: http://clusterctl-agent.clusterctl-system.svc:8080/alertmanager
        send_resolved: true
route:
  routes:
    - receiver: clusterctl
      continue: true
```

## Pause, update, remove

```sh
kubectl -n clusterctl-system scale deploy/clusterctl-agent --replicas=0   # pause (the app can do this too)
kubectl apply -f deploy/clusterctl-agent.yaml                         # update; keys are kept
kubectl delete ns clusterctl-system && kubectl delete clusterrole,clusterrolebinding clusterctl-agent
```

## Development

```sh
go test -race ./...
KUBECONFIG=~/.kube/config RELAY_URL=http://localhost:8787 go run ./cmd/clusterctl-agent   # against your current cluster
docker build -t clusterctl-agent:dev .
```

- `internal/rules`: what counts as a problem, and how it's worded.
- `internal/pairing`: keys, pairing codes and encryption.
- `internal/agent`: watching, the state ConfigMap, the relay client and the Alertmanager webhook.

The clusterctl app runs the same checks on live data, so both test against the cases in `testdata/`:

- `testdata/rules/*.json`: Kubernetes objects and the exact alerts they must produce.
- `testdata/pairing.json`: pairing codes and encrypted messages from the app, which the agent must read, and the
  reverse.

Change wording or rules there first. `deploy/clusterctl-agent.yaml` is generated from the app's installer, so the app
and this YAML install exactly the same thing. Edit it there, not by hand.

### Releases

Merging into `production` releases automatically. CI picks the version from commit titles since the last tag:

- `fix:` (or anything else) → patch,
- `feat:` → minor,
- `feat!:` → breaking.

It then tags `vX.Y.Z`, publishes `ghcr.io/razalabs/clusterctl-agent:X.Y.Z` (also `:X.Y` and `:latest`) for amd64 and
arm64, and attaches `clusterctl-agent.yaml` pinned to that image to the GitHub release. `develop` and `master` publish
`:develop` and `:master` for testing. See `.github/scripts/next-version.sh`.
