# Running in Kubernetes

The Helm chart runs ziti-stitch as a StatefulSet. Each replica is a separate instance with its own identities, as [high availability](high-availability.md) needs, and its own small volume for those identities and their renewed certificates. The stitches and settings come from the chart's values, through a ConfigMap, and changes apply without restarting anything.

Clients reach ziti-stitch over Ziti, never through Kubernetes, so the chart creates no Service you need to expose and no Ingress.

## What you need

- An identity per replica on every network the stitches use, from each network's admin (see [admin-checklist.md](admin-checklist.md)). For two replicas stitching `acme` to `partner`, that is four one-time enrollment tokens.
- OpenZiti 1.6.7 or later on every network, 2.0 or later preferred (see the [README](../README.md#requirements)).
- A storage class for small ReadWriteOnce volumes (64 MiB each by default).

## Installing

1. **Put the tokens in a Secret,** one key per replica and network, named `<pod>.<network>.jwt`. With a release called `ziti-stitch` the pods are `ziti-stitch-0`, `ziti-stitch-1` and so on:

   ```
   kubectl create secret generic ziti-stitch-enroll \
     --from-file=ziti-stitch-0.acme.jwt=acme-0.jwt \
     --from-file=ziti-stitch-0.partner.jwt=partner-0.jwt \
     --from-file=ziti-stitch-1.acme.jwt=acme-1.jwt \
     --from-file=ziti-stitch-1.partner.jwt=partner-1.jwt
   ```

   An identity that is already enrolled works too, as `<pod>.<network>.json`, if it holds its key and certificate inline (as `ziti edge enroll` writes them).

2. **Write the values:**

   ```yaml
   replicaCount: 2
   enrollment:
     secretName: ziti-stitch-enroll
   stitches:
     - name: billing
       from: {network: acme, service: billing-api}
       to: {network: partner, service: billing}
   config:
     web:
       listen: 127.0.0.1:8474   # optional; see "The web UI" below
   ```

   `stitches` takes the same fields as `stitches.yaml`, and `config` the same as `config.yaml` (see the [README](../README.md)).

3. **Install:**

   ```
   helm install ziti-stitch oci://ghcr.io/netfoundry/charts/ziti-stitch --version <version> -f values.yaml
   ```

Each pod enrolls its identities on its first start, using only the keys named after it, then runs. On later starts it uses what is on its volume and does not read the Secret, which can be deleted once every replica has started; pods keep starting without it. A token that cannot be redeemed (used already, expired) stops the pod with an error naming the network.

## Changing the stitches

Edit the values and run `helm upgrade`. The kubelet updates the ConfigMap in the pods within about a minute, and ziti-stitch applies the change on its own: unchanged stitches are not touched, changed ones restart, and a change to only `precedence` or `cost` applies in place. No pod restarts.

The stitches are read-only inside the pods: `ziti-stitch stitch ...` commands and the web UI show them but refuse to change them, saying where they are managed. Changes to `log.format`, `web.listen`, `web.ziti` and the health port take effect when the pods restart (`kubectl rollout restart statefulset/ziti-stitch`).

## Looking at it

```
kubectl logs ziti-stitch-0
kubectl exec ziti-stitch-0 -- ziti-stitch stitch list
kubectl exec ziti-stitch-0 -- ziti-stitch network test acme
```

### The web UI

With `config.web.listen: 127.0.0.1:8474` each pod serves the UI on its own loopback address. Reach it with a port forward, on the **same port number** (the UI checks the address the browser asks for):

```
kubectl port-forward pod/ziti-stitch-0 8474
kubectl exec ziti-stitch-0 -- ziti-stitch web login
```

and open the link printed by the second command.

Serving the UI over Ziti (`web.ziti`) needs a separate admin service per instance (see [web-ui.md](web-ui.md#high-availability)), and every replica in a StatefulSet gets the same configuration. With more than one replica, use the loopback UI.

## Health and upgrades

Each pod serves `/healthz` and `/readyz` on the health port (8081):

- **Startup** (`/healthz`): up to 10 minutes for the first answer. Enrolling from the Secret comes first, and each enrollment waits up to 2 minutes on its controller; liveness checks start only after this, so a slow enrollment is not cut off halfway.
- **Liveness** (`/healthz`): the process answers. A wedged pod is restarted.
- **Readiness** (`/readyz`): the configuration is valid, every network is authenticated, and every enabled stitch is listening (or gated, still serving as a last resort).

The StatefulSet upgrades pods one at a time and waits for each to be ready before the next, so an upgrade (`helm upgrade` with a new version) never takes every instance down at once. A stopping pod stops taking new connections and drains open ones for up to 30 seconds. A pod whose stitches cannot bind (a missing Bind policy, say) never becomes ready, which also stops the rollout there.

Pods are created and removed in parallel, not in order, so a replica that cannot become ready does not keep the others from starting.

**Upgrading from chart 0.1.2 or earlier:** those versions created pods in order, and Kubernetes cannot change that on an existing StatefulSet, so `helm upgrade` fails with `spec.podManagementPolicy: Invalid value: "Parallel": field is immutable`. Delete the StatefulSet (named after the release) without its pods first; the pods and their volumes keep running, and the upgrade adopts them, and replaces them one at a time as usual if the new version changes them:

```
kubectl delete statefulset ziti-stitch --cascade=orphan
helm upgrade ziti-stitch oci://ghcr.io/netfoundry/charts/ziti-stitch --version <version> -f values.yaml
```

## Scaling

- **Adding a replica:** add tokens for the new pod (`ziti-stitch-2.acme.jwt` and so on) to the Secret, then raise `replicaCount`. The existing pods are not restarted.
- **Removing one:** lower `replicaCount`. Its volume stays, with its identities; delete the PersistentVolumeClaim (`config-ziti-stitch-2`) and ask the network admins to delete those identities.
- **Starting a replica over** with new identities: delete its PersistentVolumeClaim and pod, and put fresh tokens in the Secret. The same goes for a replica whose volume is lost: it needs a new token for every network. Old tokens are spent, and an old `.json` stops working once ziti-stitch has renewed its certificate.

## Security

- The pods run as a non-root user with a read-only root filesystem, no capabilities and no service account token. ziti-stitch needs nothing from the Kubernetes API.
- The identities' private keys live on the pods' volumes. Protect the storage, and the namespace, accordingly.
- Every pod mounts the whole Secret, though each uses only the keys named after it. The tokens in it work once each; after every replica has started they are spent and the Secret can go. Tokens added for a replica that has not started yet (before scaling up) are readable, and redeemable, from every running pod until it does: add them just before raising `replicaCount`, and remove them once it has started. A `<pod>.<network>.json` holds a private key for as long as it stays there, readable by every pod; ziti-stitch warns at startup about any it does not use. Prefer tokens, and delete the Secret once every replica has started.
- The health port answers with counts only, no names.

## Without Helm

The image runs anywhere a container runs:

```
docker run -d --name ziti-stitch \
  -v ziti-stitch-config:/config \
  -v ./config:/etc/ziti-stitch:ro -e ZITI_STITCH_CONFIG_SOURCE=/etc/ziti-stitch \
  -v ./tokens:/enroll:ro \
  ghcr.io/netfoundry/ziti-stitch:<version> run --enroll-from /enroll
```

- `/config` holds the identities and their renewed certificates, so it must be writable by user 65532. A new named volume, as above, is. A bind mount must be made so: `chown 65532:65532` the host directory.
- `ZITI_STITCH_CONFIG_SOURCE` is optional. Without it, `config.yaml` and `stitches.yaml` live in `/config` too, and can be edited with `docker exec ziti-stitch ziti-stitch stitch ...`.
- `--enroll-from` enrolls `<network>.jwt` (or adds `<network>.json`) for each network not yet in `/config`. With `--enroll-for <name>` it uses only `<name>.<network>.jwt` (or `.json`), so one directory can serve several instances; the chart passes the pod's name.
