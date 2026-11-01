# DecryptMyPack

DecryptMyPack is a standalone Go service that downloads Minecraft Bedrock resource packs and serves them from local disk. The public web interface, API, pack cache, icons, and download statistics all run in one process. It does not require Cloudflare Workers/Pages, S3, an end-user account, or a subscription. In production, a reverse proxy terminates HTTPS in front of it (see Deploy).

The Minecraft client needs a Microsoft/Xbox token to join servers: either the service's own token (stored locally with mode `0600`) or a visitor's account.

### Website accounts

Visitors can optionally add their own Microsoft accounts from the homepage's account sidebar, using Microsoft's device-code page. Downloads of servers that are not cached yet then join with the visitor's selected account instead of the service token. Every finished pack still goes into the shared cache, and cached packs never need an account.

- A visitor gets an `HttpOnly`, `SameSite=Lax` cookie (`Secure` except on localhost) holding a random 32-byte secret. Nobody gets a cookie until they start adding an account.
- Each visitor's accounts and tokens are stored in one AES-256-GCM vault under `DATA_DIR/web-accounts/`. The vault key is derived from both the server master key and the visitor's cookie secret, so the files cannot be decrypted with the master key alone. File names are hashes of the secret.
- The master key comes from `ACCOUNT_KEY`, or else `DATA_DIR/web-accounts.key` is created with mode `0600`. Keep it with the data directory; losing it makes existing vaults unreadable.
- Account API calls must carry the `X-DMP-Request: 1` header and a same-origin `Origin`. A download only uses a visitor's account when the page names it in the `X-DMP-Account` header, so links or forms on other sites cannot make a visitor's account join a server.
- Limits: 5 accounts per visitor, 64 device logins pending at once. Vaults unused for 180 days are deleted at startup.

## Run locally

Requirements: Go 1.26.1 or newer.

```sh
make test
make dev
```

Open <http://localhost:8080>. Without a `token.json`, servers that are not cached yet need an account added in the page's account sidebar. To give the server its own Microsoft account instead, run `TOKEN_PATH=./token.json go run ./cmd -login`, open the printed device-code page, and restart `make dev`.

Runtime configuration:

| Variable | Default | Purpose |
| --- | --- | --- |
| `LISTEN_ADDR` | `:8080` | HTTP listen address |
| `DATA_DIR` | `data` | Local packs, icons, and statistics |
| `TOKEN_PATH` | `token.json` | Microsoft/Xbox service token |
| `MAX_CONCURRENT_DOWNLOADS` | `2` | Global download limit, from 1 to 32 |
| `ACCOUNT_KEY` | generated | Base64 32-byte master key for visitors' account vaults; defaults to `DATA_DIR/web-accounts.key` |

The public API is available at `/download`, `/status`, `/stats`, and `/browse`. The website's account endpoints live under `/api/accounts`. Generated packs are served at `/packs/...`; `/health` is the deployment health check.

## Deploy

Production runs on a single Linux host as the systemd user service in `deploy/decryptmypack.service`. A reverse proxy on that host terminates HTTPS for `decryptmypack.com` and forwards to the service. It must keep the original `Host` header, because the account API compares `Origin` against it.

### Deploy workflow

The **deploy website** workflow is started manually from GitHub Actions. It runs on a self-hosted runner on the production host (label `decryptmypack-deploy`), checks out the selected branch, runs `make test build-linux`, and calls `scripts/deploy-local.sh`. The script installs the binary and service unit, writes the host-specific listen address to `~/decryptmypack/decryptmypack.env`, restarts the user service, and checks the origin. If the restart or health check fails, it restores the previous binary, unit, and environment file. Public checks follow. Packs, account vaults, and the vault key are kept.

Host-specific values are repository secrets, never committed:

| Secret | Purpose |
| --- | --- |
| `DEPLOY_LISTEN_ADDR` | Address the service listens on, for example `127.0.0.1:8080` |
| `DEPLOY_ORIGIN` | URL the deploy script uses for origin health checks, for example `http://127.0.0.1:8080` |

The runner is started by the persistent user unit `deploy/decryptmypack-runner.service`. After registering a replacement runner with the same label, copy `bin/runsvc.sh` to the runner root, install the unit under `~/.config/systemd/user/`, and enable it with `systemctl --user enable --now decryptmypack-runner`. User lingering must be enabled so both services start at boot. Registration credentials stay outside this repository.

Without `~/decryptmypack/state/token.json`, the service runs without its own Microsoft account. Cached packs still download for everyone, but servers that are not cached yet need a visitor's account. To give anonymous visitors the shared account again, run the binary with `-login` on the host, with `TOKEN_PATH` set to that file, then restart the service.

The desktop app was retired. `/app` redirects to the homepage, and installer downloads are no longer served.
