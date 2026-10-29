# High availability

Run two or more ziti-stitch instances with the same stitches, on different machines, and the ingress network sends clients' dials to whichever instance can take them, moving them away from one that fails. Nothing is shared between instances at runtime: each one binds the ingress services and dials the egress services on its own.

## How it works

Every instance binds each stitch's ingress service, so the service has one terminator per instance. When a client dials it, the ingress controller picks a terminator by precedence first and cost second, and adds a dynamic cost to terminators whose dials fail or are slow. ziti-stitch sets its terminators' precedence and cost from each stitch's `from.precedence` and `from.cost`, and changes its own precedence when it cannot reach the egress side (see [what fails, and what clients see](#what-fails-and-what-clients-see)).

This guide assumes the ingress service uses the default terminator strategy, `smartrouting`, which raises a terminator's dynamic cost by 20 for each failed dial and by 2 for each open connection. The others behave differently: `random` ignores cost, and `sticky` keeps sending a client to the instance it used before. Ask the network admin if unsure (`ziti edge list services` shows each service's strategy).

On the egress network each instance is simply another client of the real service. Making that service itself highly available (several hosts, several terminators) is the egress network's business and works as for any other client.

## Setting it up

1. **One identity per instance, on every network.** Ask each network's admin for an identity per instance, all with the same role attribute so that one set of policies covers them (see [admin-checklist.md](admin-checklist.md)):

   ```
   ziti edge create identity stitch-1 -a stitch-hosts -o stitch-1.jwt
   ziti edge create identity stitch-2 -a stitch-hosts -o stitch-2.jwt
   ```

   Do not copy one identity's file to several instances. ziti-stitch renews certificates by itself, and the moment one instance renews, the certificate in every other copy stops working. Separate identities also let an admin tell instances apart and revoke one on its own.

2. **The same networks and stitches on every instance.** Enroll each instance's identities under the **same network names** on every instance (`--name acme`), then give every instance the same `stitches.yaml`. The only files that differ between instances are those in `networks/`.

3. **Install each instance as a service** as described in [install.md](install.md), and check each with `ziti-stitch validate`.

### Keeping instances in step

`stitches.yaml` is a plain file, and ziti-stitch applies changes to it within a second, so keep one copy under version control and push it to every instance with whatever you already use (Ansible, Salt, a GitOps agent, a shared read-only volume). Changes made with `ziti-stitch stitch ...` or in the web UI change only the instance they ran on; with several instances, make changes in the shared copy instead.

`config.yaml` can differ per instance, for example to bind each instance's web UI as its own service (see [web-ui.md](web-ui.md#high-availability)).

While a change is rolling out, instances briefly run different stitches. A stitch that only some instances have yet is served by those; one that some have already removed is served by the rest.

## Active/active, the recommended setup

Give every instance's stitches the same precedence and cost, which is what you get by not setting either. Any instance may get a given dial: the ingress controller sends each to the terminator it rates cheapest, which with equal costs comes down to recent failures and how busy each one is. Do not count on an even split. When one instance fails, the others take its dials. This setup handles every failure in the next section best, including the hardest one, an instance that stops answering without disconnecting.

## Preferring one instance

To have one instance take the dials while it is healthy and another only when it is not, give the preferred instance a lower **cost**, and leave precedence alone:

```yaml
# on the preferred instance            # on the other one
from: {network: acme, service: x}      from: {network: acme, service: x, cost: 10}
```

Cost and precedence changes apply to running instances in place, without a rebind, so you can move traffic between instances by editing them. Keep the gap at **10 or less**. The controller moves dials away from a failing instance by adding to its dynamic cost, about 20 for each failed attempt. With a gap smaller than that, a dial that fails on the preferred instance is retried on the other one and succeeds, and clients see a slow dial rather than a failed one. With a larger gap, retries go back to the preferred instance, and dials fail until its dynamic cost has grown past the gap: with a gap of 500, a preferred instance that had stopped answering kept failing every dial for over two minutes.

Do **not** use `precedence: required` to pick a primary. Precedence beats cost, so a required instance gets every dial for as long as its terminators exist. ziti-stitch lowers its own precedence when it cannot reach the egress side, but an instance that has hung, or whose machine has dropped off the network, can do nothing, and its terminators stay. In testing, every dial failed for as long as such a required instance stayed frozen, and the healthy standby got none of them.

## What fails, and what clients see

| What happens | What clients see |
|--------------|------------------|
| An instance stops, restarts or crashes | Nothing. Its terminators go when its connections to the routers close, and new dials go to the others at once. Connections open through it end: at once on a crash, after up to 30 seconds of draining on a stop |
| An instance cannot reach the egress service | A failed dial, then the others take over. On its first failure, that instance checks the egress service with a dial of its own (one failure may be down to that client's app data). If that fails too, it lowers its own precedence to `failed`, and raises it again once the egress service answers (it checks every 1 to 30 seconds). The change takes a moment to reach the controller, so under heavy traffic the dials arriving in that moment fail too. With `annotate`, the check dials name the stitch but no client |
| Every instance cannot reach the egress service | Dials fail with a generic error until the egress service is back. A failed instance is still used when there is nothing better |
| An instance hangs, or its machine drops off the network | With equal costs or a gap of 10 or less: no failed dials, but every minute or two one dial takes 5 seconds longer, while the router waits for the instance before the controller retries elsewhere. With a larger gap or `required`: failed dials, see above |

An instance that hangs or vanishes keeps its terminators until the router's connection to it closes. A router (as of OpenZiti 2.0) does not check that the SDKs hosting services on it are still answering, so that takes as long as TCP does to give up on the connection, with Linux's defaults on the router's machine:

- **No traffic:** about 2.5 minutes, the router's TCP keepalive (15 seconds, then 9 probes 15 seconds apart). Measured: 157 seconds.
- **Traffic flowing:** about 15 minutes. Dials the router sends to the lost instance go unacknowledged, and TCP retransmits them until it gives up (`net.ipv4.tcp_retries2`, 15 by default). Measured: 964 seconds, during which a client dialing every 2 seconds saw 487 dials succeed, none fail, and 9 take 5 seconds.
- **The process frozen but its machine up:** never; the machine's TCP keeps answering.

Until then the controller keeps the terminator, and it is dynamic cost that steers dials away from it. A router's admin can shorten the traffic case by lowering `net.ipv4.tcp_retries2` on the router's machine (8 gives about 100 seconds), at the cost of dropping connections sooner on lossy links.

## Upgrades and maintenance

Upgrade or restart one instance at a time. A stopping instance stops taking new dials at once and gives open connections up to 30 seconds to finish, so with another instance up, clients see no interruption. Start it again and check it is serving (`ziti-stitch service status`, or its web UI) before moving on to the next.

To take an instance out of service without stopping it, disable its stitches on that instance only (`ziti-stitch stitch disable <name>`). Remember to enable them again, and not to push a shared `stitches.yaml` over the top in the meantime.

## Watching it

Each instance reports on its own: its web UI or logs show each stitch as `listening` or `gated` (cannot reach the egress side, precedence lowered), with dial and failure counts. A network admin can see every instance's terminators on a service, with their precedence and dynamic cost:

```
ziti edge list terminators 'service.name="billing-api"'
```

A terminator at precedence `failed` belongs to an instance that cannot reach the egress side. A high dynamic cost on a terminator whose instance looks fine is a sign the instance is not answering.

## Where to run instances

Each stitch adds a second trip across a Ziti network: client to instance on the ingress side, instance to service on the egress side. As a rough guide from a single-machine test (both networks, the stitch and the endpoints on one host): a stitch kept 80 to 90% of the bulk throughput of a direct connection, and added about 1.7 ms to setting up each connection, for the egress dial it makes before accepting. Real numbers depend on the routers and the links between them. Run instances close to edge routers of both networks, and run them on different machines, ideally in different failure domains (racks, zones, sites), so that one outage does not take them all.
