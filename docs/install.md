# Installing ziti-stitch

ziti-stitch is a single binary. Install it somewhere permanent, enroll an identity for each network, add your stitches, and install it as a system service.

Before you start, get what you need from each network's admin: see [admin-checklist.md](admin-checklist.md).

## Downloading

Each release has an archive per platform and a `checksums.txt`. Check the archive against it before unpacking:

```
sha256sum -c checksums.txt --ignore-missing                          # Linux
shasum -a 256 -c checksums.txt --ignore-missing                      # macOS
(Get-FileHash ziti-stitch_<version>_windows_amd64.zip).Hash.ToLower()  # Windows: compare with checksums.txt
```

The binaries are not code-signed yet, so the first run may be blocked:

- **macOS** refuses to open a binary downloaded with a browser ("cannot be opened because the developer cannot be verified"). Clear the download flag once: `xattr -d com.apple.quarantine ziti-stitch`. Files fetched with `curl` are not flagged.
- **Windows** may show "Windows protected your PC" (SmartScreen) or refuse to run a downloaded file. Unblock it once from PowerShell: `Unblock-File .\ziti-stitch.exe`.

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

Open the link it prints; it works once, for five minutes. The loopback listener only answers on this machine. To reach the UI from elsewhere, over an SSH port forward or as a Ziti service with per-identity access, see [web-ui.md](web-ui.md).

## Upgrading

```
ziti-stitch service stop
# replace the binary
ziti-stitch service start
```

Stopping drains open connections for up to 30 seconds. For no interruption at all, run two instances (see [high-availability.md](high-availability.md)) and upgrade them one at a time.

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
