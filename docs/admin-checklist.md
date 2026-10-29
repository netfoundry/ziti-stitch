# What to ask your network admins for

ziti-stitch never needs admin rights on any network. Everything it needs is created by each network's admin, once. Send them the section for their side.

The examples use the `ziti` CLI and role attributes; the admin console works just as well. Names are examples.

**Versions.** The controller and routers need OpenZiti 1.6.7 or later. On the ingress network, 2.0 or later also lets ziti-stitch see which identity is calling; on 1.6 everything else works the same.

## Ingress network (where the clients are)

ziti-stitch hosts the service here, the way a tunneler or SDK app hosting a service would.

1. **An identity for each ziti-stitch instance**, with a one-time enrollment token (JWT), and a role attribute shared by all of them:

   ```
   ziti edge create identity stitch-1 -a stitch-hosts -o stitch-1.jwt
   ```

   Run two or more instances for high availability; give each its own identity with the same attribute (see [high-availability.md](high-availability.md)).

2. **The service** the clients will dial, if it does not exist yet. ziti-stitch needs no configs on it. Clients that intercept traffic (tunnelers) need their usual `intercept.v1` config; do not add a `host.v1` config for ziti-stitch's sake.

   ```
   ziti edge create service billing-api -a stitched
   ```

3. **A Bind service policy** for the ziti-stitch identities on that service:

   ```
   ziti edge create service-policy stitch-hosts-bind Bind --identity-roles '#stitch-hosts' --service-roles '#stitched'
   ```

4. **A Dial service policy** for the clients, as for any service. Every identity it admits can open connections through ziti-stitch, and by default they stay open, idle or not, with no limit on how many. If that includes identities you do not fully trust, ask whoever runs ziti-stitch to set `max_connections` and `idle_timeout` on the stitch.

5. **Edge router access**: an edge router policy that lets the ziti-stitch identities use routers that serve the service, and a service edge router policy for the service. Many networks already have "all" policies that cover this.

## Egress network (where the real service is)

ziti-stitch is a client here, like any SDK app dialing a service.

1. **An identity for each ziti-stitch instance**, with an enrollment token and a shared attribute:

   ```
   ziti edge create identity stitch-1 -a stitch-dialers -o stitch-1.jwt
   ```

2. **A Dial service policy** for those identities on the real service:

   ```
   ziti edge create service-policy stitch-dialers-dial Dial --identity-roles '#stitch-dialers' --service-roles '@billing'
   ```

3. **Edge router access**, as above.

Whoever hosts the real service needs no changes. If it is a tunneler that forwards to the address the client intercepted (`forwardAddress` etc. in `host.v1`), it relies on the client's app data, which ziti-stitch passes through unchanged by default.

If the stitch is set to `annotate`, the app data also carries a `ziti_stitch` object naming the stitch and the original caller. ziti-stitch replaces any `ziti_stitch` object the client sent, and removes keys a JSON parser might take for it (differing only in case, for example); it refuses dials whose app data is not a JSON object. Trust the object only from a stitch you know annotates: with the default, passthrough, it is whatever the client sent.

## The web UI over Ziti

Only if ziti-stitch's web UI is to be reached as a Ziti service. On the network named in `web.ziti.network`: a service with an `intercept.v1` config for the address people will use, a Bind policy for the ziti-stitch identities, and a Dial policy for the people who should reach it. The commands, and what the Dial policy does and does not grant, are in [web-ui.md](web-ui.md#what-the-network-admin-creates).

## Things to avoid on either side

- **MFA posture checks** on policies covering ziti-stitch identities. A service cannot type a TOTP code.
- **Posture checks in general**, unless you have confirmed ziti-stitch passes them. They have not been tested with ziti-stitch yet.
- **Short-lived enrollment tokens** if the person enrolling will not get to it soon. Tokens work once and expire.

## Certificates

ziti-stitch renews its identities' certificates by itself and saves the new ones in its config. It renews 30 days before a certificate expires, or sooner for a short-lived one: once a third of its lifetime is left (10 days for a 30-day certificate). It checks every hour, and only for networks it is using. A network that no enabled stitch (or the web UI) uses is not running, so its certificate is not renewed and can expire. An admin can ask for a renewal sooner (`ziti edge update authenticator cert <id> --request-extend`); ziti-stitch picks the request up within an hour.

## Checking the result

Once the identities are enrolled and the stitches added, `ziti-stitch validate` confirms every Bind and Dial is in place. When one is missing it says which, e.g.:

```
problem  stitch billing: identity "stitch-1" can dial acme:billing-api but not bind it; it needs a Bind service policy
```
