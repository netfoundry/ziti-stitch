# Installing ziti-stitch

ziti-stitch is a single binary. Install it somewhere permanent, enroll an identity for each network, add your stitches, and install it as a system service.

Before you start, get what you need from each network's admin: see [admin-checklist.md](admin-checklist.md).

## Linux (systemd)

```
sudo install -m 0755 ziti-stitch /usr/local/bin/

sudo ziti-stitch enroll --jwt acme.jwt                  # name derived from the controller host
sudo ziti-stitch enroll --jwt partner.jwt --name partner
sudo ziti-stitch stitch add billing --from acme:billing-api --to partner:billing
sudo ziti-stitch validate

sudo ziti-stitch service install
sudo ziti-stitch service start
```

Running the CLI as root puts the config in `/var/lib/ziti-stitch`, which is where the service looks. Use `--config-dir` on every command (and on `service install`) to put it somewhere else.

- Logs: `journalctl -u ziti-stitch -f`
- Status: `ziti-stitch service status`, or `systemctl status ziti-stitch`
- Config changes made with the CLI apply on their own; `systemctl reload ziti-stitch` forces a reload.

The unit is locked down: no capabilities, a read-only filesystem apart from the config directory, no access to devices or kernel settings, and only IP, Unix and netlink sockets. ziti-stitch only makes outbound connections, so it needs nothing more. It runs as root inside that sandbox by default. To run it as a dedicated user instead:

```
sudo useradd --system --home-dir /var/lib/ziti-stitch --shell /usr/sbin/nologin ziti-stitch
sudo chown -R ziti-stitch: /var/lib/ziti-stitch
sudo ziti-stitch service install --user ziti-stitch
```

From then on run the CLI as that user (`sudo -u ziti-stitch ziti-stitch --config-dir /var/lib/ziti-stitch ...`), so that the files it writes stay readable by the service.

## macOS (launchd)

```
sudo install -m 0755 ziti-stitch /usr/local/bin/
sudo ziti-stitch enroll --jwt acme.jwt
...
sudo ziti-stitch service install
sudo ziti-stitch service start
```

- Config: `/Library/Application Support/ziti-stitch`
- Logs: `/Library/Logs/ziti-stitch/ziti-stitch.log` (rotated at 10 MB, 5 kept), also visible in Console.app. `ziti-stitch.err.log` next to it catches anything printed before logging starts, such as a crash.
- launchd restarts ziti-stitch if it exits and starts it at boot.

## Windows

From an Administrator PowerShell, with `ziti-stitch.exe` in e.g. `C:\Program Files\ziti-stitch\`:

```
ziti-stitch enroll --jwt acme.jwt
...
ziti-stitch service install
ziti-stitch service start
```

- Config: `%ProgramData%\ziti-stitch`. Its access list is replaced so that only SYSTEM and Administrators can read it; the identity files hold private keys.
- Logs: `%ProgramData%\ziti-stitch\logs\ziti-stitch.log` (rotated at 10 MB, 5 kept).
- The service runs as LocalSystem, starts at boot and restarts 5 seconds after a failure.
- There is no SIGHUP on Windows; config changes are picked up from the files directly.

## The web UI

Off by default. To turn it on, add this to `config.yaml` in the config directory and restart the service:

```yaml
web:
  listen: 127.0.0.1:8474
```

Then, on the same machine:

```
sudo ziti-stitch web login      # add --config-dir if the service uses one
```

Open the link it prints; it works once, for five minutes. The loopback listener only answers on this machine; for other machines, serve the UI over Ziti, or forward the port over SSH (`ssh -L 8474:127.0.0.1:8474 host`).

### Over Ziti

ziti-stitch can bind the UI as a Ziti service through one of its networks' identities, so that people reach it from their own tunneler like any other service. Ask that network's admin for the service and its policies (see [admin-checklist.md](admin-checklist.md#the-web-ui-over-ziti)), then:

```yaml
web:
  ziti:
    network: acme
    service: stitch-admin
    hosts: [stitch-admin.ziti]     # the intercept address people will use
    allow: [Xy7rT2kQp]             # identity ids with full access
    policy_access: read            # everyone else the service policy admits may look
```

- `allow` takes identity **ids**, not names: the router vouches for the id of whoever dialed, and an admin could rename an identity. Someone without access who opens the UI sees their identity's id on the sign-in page, ready to copy.
- `policy_access` is `none` by default. `read` lets anyone the service policy admits see everything and test networks, but not change stitches. `edit` hands full control to whoever that network's admin lets dial the service; only use it when that admin is you.
- Changing `allow` or `policy_access` takes effect at once, revoking included. Changing the network, service or hosts needs a restart.
- Requests naming a host other than `hosts` are refused. That is what stops a malicious web page from using a visitor's tunneler to reach the UI.

## Upgrading

```
ziti-stitch service stop
# replace the binary
ziti-stitch service start
```

Stopping drains open connections for up to 30 seconds. For no interruption at all, run two instances (see the HA guide) and upgrade them one at a time.

## Removing

```
ziti-stitch service stop
ziti-stitch service uninstall
```

The config directory is left in place. Delete it to remove the identities' private keys, and ask the network admins to delete the identities on their controllers.

## Troubleshooting

- `ziti-stitch validate` checks the config and, for every stitch, that its identities have the Bind and Dial permissions they need. Each problem names the policy that is missing.
- `ziti-stitch network test <name>` lists what one identity can see.
- An invalid config does not stop the service: it logs the problem and runs nothing until the config is fixed. Fix it and the change is picked up.
