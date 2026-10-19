# fleet-manager

One small Go program that runs a fleet of personal web apps on a single server. It
deploys every push, watches every app, restarts the ones that fall over, and shows it
all on a dashboard.

## What it does

- **Deploys every push.** Each app's GitHub build publishes a release. GitHub tells
  fleet-manager through a signed webhook (with a check every 10 minutes as a fallback). It
  downloads the build, checks it against the published SHA-256, installs it, restarts
  the app and waits for it to report healthy.
- **Rolls back bad releases.** If the new version does not come up healthy, the
  previous build is put back. A failed release is retried four more times within about two minutes,
  then again after 15 minutes, 1 hour, 3 hours and 6 hours, before it is given up on.
- **One deploy at a time.** Releases that arrive together queue up instead of starting
  several apps at once on a small machine.
- **Updates itself safely.** When fleet-manager deploys a new version of itself, a
  separate self-check restores the old one if the new one does not start.
- **Restarts failed apps.** The watchdog restarts any app that has been down for 30
  seconds and sends a phone alert through [ntfy](https://ntfy.sh).
- **Dashboard.** Memory and CPU per app, recent builds and deploys per app with how long
  each step took, and AI coding-session usage, behind a shared login.

## Layout

| Folder | What is in it |
|---|---|
| `deployer/` | Webhook, download, checksum, install, health check, rollback, retries, login check |
| `watchdog/` | Restarts apps that stay down |
| `collector/`, `metrics/` | Memory and CPU readings for the dashboard |
| `otel/` | Receives usage data from AI coding sessions |
| `canary/` | Warns when an external API an app depends on changes shape |
| `ntfy/` | Sends phone alerts |
| `static/` | Dashboard pages |

## Settings

| Setting | What it is for |
|---|---|
| `SSO_SIGNING_KEY` | Key that signs the shared login cookie. Without it nobody can log in. |
| `NTFY_URL` | ntfy topic for alerts. Without it alerts are only logged. |
| `deploy-config.json` | The apps to deploy, the GitHub token and the webhook secret (not in this repo). |

## Running the tests

```
go test ./...
```
