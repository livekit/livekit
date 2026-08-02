# Buildkite CI/CD

CI/CD runs on Buildkite (org `gusto`), pipeline slug **`tax-credits-livekit-server`**.
`pipeline.yml` defines the steps; the CI step runs hermetically via the `docker`
plugin on the shared `default_stable` queue, matching
`ardiustech/watercooler`'s own pipeline conventions.

Before this pipeline existed, this repo had **no CI/CD at all**: the inherited
upstream `.github/workflows/buildtest.yaml` isn't wired in as a required
status check (see "Also fix: make buildtest.yaml a required check" below),
and deploy was a fully manual SSM runbook (see
`ardiustech/watercooler`'s `infrastructure/livekit/README.md`, pre-this-pipeline
version of "Rolling the LiveKit version").

## What runs

| Step | Image | Does |
|---|---|---|
| build · vet · test | `golang:1.26` | `gofmt -l` (fails on any diff), `go build ./...`, `go vet ./...`, `go test ./...` |
| deploy (graceful drain) | agent Docker | **master only**, after the above pass: build+push amd64 image, trigger on-box graceful-drain deploy via SSM |

The verify step needs no credentials. The deploy step (`.buildkite/steps/deploy.sh`)
needs the CI AWS keys as **`LK_AWS_*`** secrets (below); without them it
**soft-skips** (green, no deploy) — same pattern as watercooler's own `WC_AWS_*`.
Named `LK_AWS_*` (not `WC_AWS_*` or bare `AWS_*`) because this is a **different
IAM user**, scoped to only the `livekit-server` ECR repo and the LiveKit
instance — it has no access to anything watercooler's own CD user can touch,
and vice versa.

## CD / auto-deploy

On merge to `master`, the deploy step builds the amd64 image (tagged by commit
SHA + `latest` — amd64 only, since the LiveKit box is a c6i x86_64 instance,
not arm64 like the watercooler app box), pushes to ECR, and runs
`/opt/livekit/deploy.sh <sha>` on the instance via SSM.

**This is NOT a blue/green swap like watercooler's own app deploy.** LiveKit
runs `network_mode: host` bound to fixed ports (7880 signaling, 7881/7882
media) — two copies can't bind those simultaneously on one box, so there's no
second color to flip to. Instead, the on-box script leans on **LiveKit's own
built-in graceful shutdown**: `cmd/server/main.go`'s SIGTERM handler calls
`router.Drain()` (stop accepting new joins) then waits for every active
participant to leave before actually exiting — this is not something this
pipeline invented, it's already how the upstream binary behaves. A deploy
therefore:
- does **not** forcibly drop any call already in progress (the old process
  waits for it to end naturally, up to a generous bound — see `DRAIN_TIMEOUT`
  in the on-box `deploy.sh`);
- **does** pause new joins for the drain window, since there's nowhere else
  for them to go on a single-node SFU.

True zero-downtime (new joins routed to an already-warm second node while the
old one drains) needs a second EC2 node + Redis-backed multi-node LiveKit + a
router in front — a separate, materially bigger infra project, not something
this pipeline does. See `ardiustech/watercooler`'s
`infrastructure/livekit/README.md` for the full tradeoff writeup.

### Activate (one-time)

1. `cd infrastructure/livekit && terraform apply` (in `ardiustech/watercooler`)
   — creates the `watercooler-livekit-ci-<env>` IAM user (ECR push to the
   `livekit-server` repo + SSM `SendCommand` to the livekit instance only; no
   EC2/Terraform access) and the `livekit-server` ECR repository itself.
2. Read its key:
   ```bash
   terraform -chdir=infrastructure/livekit output -raw livekit_ci_access_key_id
   terraform -chdir=infrastructure/livekit output -raw livekit_ci_secret_access_key
   ```
3. Add them as pipeline env vars in AWS Secrets Manager (same mechanism
   watercooler's own pipeline uses — see its `.buildkite/README.md` for the
   exact account/region/secret-name convention): secret
   `buildkite/tax-credits-livekit-server/environment`, plaintext `KEY=value`
   lines: `LK_AWS_ACCESS_KEY_ID`, `LK_AWS_SECRET_ACCESS_KEY` (optional:
   `LK_AWS_REGION` default `us-west-2`, `ECR_REPO`, `INSTANCE_NAME_TAG`,
   `DEPLOY_ACCOUNT_ID`).

Until step 3 is done the deploy step soft-skips, so merges stay green.

**Separately, and just as important:** even once this deploys, the fork is
still a no-op in production until:
- `infrastructure/livekit/terraform.tfvars`'s `livekit_image` is switched from
  the stock `livekit/livekit-server:vX.Y.Z` image to this repo's ECR
  repository — a deliberate go-live decision, not a side effect of this
  pipeline existing (matches the project's existing philosophy — see that
  repo's `ARDIUSTECH_FORK.md`);
- `rtc.reconnect_on_publication_error: true` is set — this pipeline's
  companion change bakes it into the LiveKit config template by default, but
  it only takes effect once the box actually boots that config (a fresh
  instance, or an intentional `user_data` roll).

### Rollback

Images are tagged by commit SHA. To roll back, run the previous SHA on the box:
```bash
aws ssm send-command --document-name AWS-RunShellScript \
  --targets "Key=tag:Name,Values=watercooler-livekit-prod" \
  --parameters 'commands=["/opt/livekit/deploy.sh <previous-sha>"]' \
  --profile ardius-admin-ardius-dev --region us-west-2
```

### Also fix: make `buildtest.yaml` a required check

Separate from this Buildkite pipeline: the inherited upstream
`.github/workflows/buildtest.yaml` (Go tests + a Redis service container) runs
on every push/PR to `master` today but isn't a **required** status check, so
a PR can merge without it having passed (flagged in PR #1's third
adversarial-review round). Once this pipeline's own PR merges, add both
`test` (from `buildtest.yaml`) and this pipeline's checks as required status
checks on `master` via branch protection — needs a GitHub admin on the
`ardiustech` org (not something a repo-scoped token can set).

## One-time setup (requires Buildkite admin / `write_pipelines`)

1. **Buildkite → Add pipeline.**
   - Name / slug: `tax-credits-livekit-server`
   - Repository: `git@github.com:ardiustech/livekit-server.git`
   - Cluster/queue: the one that provides `default_stable` agents (matches
     mithrin / watercooler).
2. **Initial step** (the only step configured in the UI):
   ```
   buildkite-agent pipeline upload
   ```
   Everything else is read from `.buildkite/pipeline.yml` in the repo.
3. **Builds on PRs:** enable "Build pull requests" so PRs get checked.

## Inspecting builds (bk CLI)

```bash
bk build list -p tax-credits-livekit-server
bk build view  -p tax-credits-livekit-server <number>
bk job log <job-id> -p tax-credits-livekit-server -b <number>
```
