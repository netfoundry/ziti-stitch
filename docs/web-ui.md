# Making the web UI available

The web UI is optional and off by default. It shows the stitches, networks, open connections and recent events, lets you add, edit, enable, disable and remove stitches, and tests what each network's identity can do. Enrolling stays in the CLI.

There are no passwords and no accounts. Who gets in depends on how they reach the UI:

| Path | Who can reach it | How they get in | Set up by |
|------|------------------|-----------------|-----------|
| [Loopback](#loopback) | people on the machine running ziti-stitch | a one-time login link | you, in `config.yaml` |
| [SSH port forward](#ssh-port-forward) | anyone who can SSH to that machine | a one-time login link | you |
| [Over Ziti](#over-ziti) | identities a network's service policy lets dial the UI's service | their identity, a login link, or both | you, plus that network's admin |

Loopback and Ziti can both be on at once. They are separate listeners, so if one cannot start (a port in use, say) the other still serves and the stitches carry on.

## Loopback

```yaml
web:
  listen: 127.0.0.1:8474
```

Add this to `config.yaml` in the config directory and restart ziti-stitch. `listen` must be a loopback address (`127.0.0.1`, `::1` or `localhost`): ziti-stitch does not serve the UI on a network interface.

To sign in, run this on the same machine, as a user who can change the configuration:

```
sudo ziti-stitch web login      # add --config-dir if the service uses one
```

It prints a link. The link works once, within five minutes, and signs you in for 12 hours with full access. Only someone who can write the config directory can make a link, which is exactly who could change the configuration by hand anyway, so the link adds no new way in.

Signing out ends that browser's session. To sign everybody out, delete `web/session.key` in the config directory and restart ziti-stitch.

## SSH port forward

For a machine you can SSH to, forward the loopback port:

```
ssh -L 8474:127.0.0.1:8474 admin@stitch-host
```

Then run `sudo ziti-stitch web login` in that SSH session and open the link on your own machine.

Use the **same port number** on both ends. The UI only answers requests addressed to its own address (see [the Host check](#the-host-check)), and a browser pointed at `127.0.0.1:9000` says so in every request.

## Over Ziti

ziti-stitch can bind the UI as a Ziti service with one of its networks' identities. People then reach it from their own tunneler like any other service, from wherever they are, with no inbound port open anywhere.

### What the network admin creates

On the network named in `web.ziti.network`:

1. **The service**, with an `intercept.v1` config giving the address people will use. Port 80 keeps the address short:

   ```
   ziti edge create config stitch-admin-intercept intercept.v1 '{"protocols":["tcp"],"addresses":["stitch-admin.ziti"],"portRanges":[{"low":80,"high":80}]}'
   ziti edge create service stitch-admin --configs stitch-admin-intercept -a stitch-admin
   ```

   No `host.v1` config: ziti-stitch hosts the service itself.

2. **A Bind service policy** for ziti-stitch's identity on that network. The identity that already binds the stitched services will do:

   ```
   ziti edge create service-policy stitch-admin-bind Bind --identity-roles '#stitch-hosts' --service-roles '#stitch-admin'
   ```

3. **A Dial service policy** for the people who should reach the UI:

   ```
   ziti edge create service-policy stitch-admin-dial Dial --identity-roles '#stitch-admins' --service-roles '#stitch-admin'
   ```

This policy decides who can reach the UI. What they may do once there is ziti-stitch's own setting, described next.

### The config

```yaml
web:
  ziti:
    network: acme                  # the network whose identity binds the service
    service: stitch-admin
    hosts: [stitch-admin.ziti]     # the intercept address(es)
    allow: []                      # identity ids with full access
    policy_access: none            # none, read or edit
```

- `hosts` is required, and lists the intercept addresses. Add the port if it is not 80, e.g. `stitch-admin.ziti:8080`. Requests naming any other host are refused.
- `allow` takes identity **ids**, not names. The router tells ziti-stitch the id of whoever dialed, verified from their session, so it cannot be forged; a name can be changed by an admin at any time.
- `allow` needs the network to run OpenZiti 2.0 or later: older routers do not pass on who dialed, so nobody matches. On 1.6, use login links or `policy_access`.
- `policy_access` is what everyone else the Dial policy admits gets. It defaults to `none`.

Changes to `allow` and `policy_access` apply at once, to the very next request, revoking included. There is no session to wait out. Changing `network`, `service` or `hosts` needs a restart.

### Access levels

| Level | What it allows |
|-------|----------------|
| none | the sign-in page only |
| read | every page, and testing networks; no changes to stitches |
| edit | everything the UI can do |

Someone with **read** sees the stitch settings greyed out, a "read-only" badge next to their name, and no add, edit or remove buttons. The server refuses those changes from them whatever their browser sends.

Someone with **none** sees the sign-in page, naming the identity they came in as and its id, ready to copy into `allow`.

### Choosing who gets in

**Login links only** (the default). The Dial policy lets people reach the UI; nobody gets further without a login link from someone on the ziti-stitch machine.

```yaml
# in web.ziti:
allow: []
policy_access: none
```

`ziti-stitch web login` prints a link for each listener; the Ziti one uses the first entry in `hosts`. Open it through your tunneler. Two people have to agree to let someone in: the network's admin (the Dial policy) and someone who can change ziti-stitch's configuration (the link). This suits a UI bound on a network someone else runs.

**An allowlist.** The identities in `allow` get full access with no sign-in step. Everyone else the Dial policy admits gets the sign-in page.

```yaml
# in web.ziti:
allow: [Xy7rT2kQp, Bm4WqL9zA]
policy_access: none
```

To find an id, have the person open the UI: the sign-in page shows it. Or look it up on the controller (`ziti edge list identities 'name="alice"'`).

**Read for the team, edit for a few.** Anyone the Dial policy admits can look; the allowlist (or a login link) is needed to change anything. Good for an operations team that needs to see what is happening.

```yaml
# in web.ziti:
allow: [Xy7rT2kQp]
policy_access: read
```

**The Dial policy decides everything.** Anyone the Dial policy admits gets full access. Who controls the stitcher is then managed entirely on the controller, with the same role attributes as everything else on that network.

```yaml
# in web.ziti:
policy_access: edit
```

Only do this on a network you administer. With `edit`, whoever administers that network's policies can give anyone, themselves included, control of ziti-stitch, and through it of what crosses into your other networks.

### Choosing the network

The UI can be bound on any network ziti-stitch has an identity for. Pick one you, or people you trust with the stitcher, administer:

- Its admin decides who can reach the UI at all.
- With `policy_access: read`, they decide who can see your stitches, networks and connections.
- With `policy_access: edit`, they decide who controls ziti-stitch.

On a network someone else runs, keep `policy_access: none` and use `allow` or login links. Their admin can then stop people reaching the UI, but cannot let anyone in.

The network named in `web.ziti.network` keeps running even when no stitch uses it, and `ziti-stitch network remove` and `network rename` refuse to touch it until `config.yaml` stops naming it.

## Security model

- **Login links** are random 256-bit tokens. The config directory holds only their hashes, each link works once, and an unused one expires after five minutes.
- **Sessions** are cookies signed with `web/session.key`, valid for 12 hours, `HttpOnly` and `SameSite=Strict`. A session started by a Ziti identity only works on connections from that same identity.
- **Every change** (a POST) needs the session's CSRF token, over Ziti as well as on loopback.
- **Every change is logged** with who made it: `by=alice (Xy7rT2kQp)`, or `by="login link"`. The events page shows the same.
- **Pages** carry a strict Content Security Policy (no inline scripts or styles, nothing loaded from elsewhere), `Cache-Control: no-store`, and are never framed.
- **Identity files and keys** never appear in the UI.

The UI is plain HTTP. On loopback the traffic never leaves the machine. Over Ziti it is end-to-end encrypted by Ziti from the user's tunneler to ziti-stitch; the only unencrypted hop is from the browser to the tunneler on the user's own machine.

### Why CSRF and the Host over Ziti

A tunneler adds its identity to everything that goes through it, so to the UI, every request from a user's machine looks like it came from that user. That includes requests a malicious web page makes in the background while the user has it open:

- Without the CSRF token, a page could submit a form to `http://stitch-admin.ziti/` and change stitches as the user. It cannot read the token, so it cannot.
- Without the Host check, a page could point its own domain at the intercept address (DNS rebinding) and read the UI as the user. Its requests name its own domain, so they are refused.

The same two checks protect the loopback listener from pages that point their domain at `127.0.0.1`.

### The Host check

The loopback listener answers to its `listen` address, and to `localhost`, `127.0.0.1` and `[::1]` on the same port. The Ziti listener answers to the names in `hosts`, in any letter case. A request naming anything else gets `421 unexpected Host header`.

That is also why the UI does not work behind a **reverse proxy** or a port forward that changes the port: the browser names the proxy's address. This is not supported.

## High availability

When you run two or more ziti-stitch instances for the same stitches (see [high-availability.md](high-availability.md)), give each instance's UI **its own service**, `stitch-admin-1`, `stitch-admin-2` and so on, with their own intercept addresses:

```yaml
# instance 1                       # instance 2
web:                               web:
  ziti:                              ziti:
    network: acme                      network: acme
    service: stitch-admin-1            service: stitch-admin-2
    hosts: [stitch-1.admin.ziti]       hosts: [stitch-2.admin.ziti]
```

Do not have several instances bind one admin service. Ziti would spread each person's requests across them, and:

- each instance signs sessions with its own key, so a session from one is refused by the next, and you are sent back to sign in;
- a change made in the UI only changes the instance that served it, so you cannot tell which instance you changed, and their configs drift apart.

A role attribute on the services (`-a stitch-admin`) lets one Bind and one Dial policy cover them all.

Changes made in one instance's UI stay on that instance. Keeping instances' configurations in step is covered in [high-availability.md](high-availability.md#keeping-instances-in-step).

## Troubleshooting

| Symptom | Cause |
|---------|-------|
| `web login` says the UI is off | neither `web.listen` nor `web.ziti` is set in `config.yaml`, or `--config-dir` points elsewhere |
| the link says it is invalid or expired | it was used already, or is over five minutes old; run `web login` again |
| `421 unexpected Host header` | the address in the browser is not one the UI answers to: check `hosts`, or the local port of an SSH forward |
| the sign-in page names your identity and says it has no access | add its id to `allow`, or set `policy_access`, or use a login link |
| the sign-in page says the network did not say which identity you are | the network runs OpenZiti before 2.0; use `policy_access` or a login link |
| the Ziti address does not resolve or connect | the user's identity cannot dial the service (the Dial policy), or the tunneler is not running |
| log: `cannot listen for the web UI on loopback` | the port is in use by something else; pick another `listen` port and restart |
| log: `cannot bind the web UI's Ziti service; retrying` | ziti-stitch's identity has no Bind policy for the service, or the service name is wrong; it retries on its own, up to every 30 seconds |
| changes to `hosts` or `service` do nothing | they need a restart; `allow` and `policy_access` do not |
