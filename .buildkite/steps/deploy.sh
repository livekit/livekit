#!/bin/bash
# CD: build + push the amd64 image (tagged by commit SHA + latest), then trigger
# the on-box graceful-drain deploy via SSM and wait for it. Runs on the agent's
# Docker (needs buildx). Mirrors ardiustech/watercooler's own
# .buildkite/steps/deploy.sh structure closely — see that file's comments for
# the AWS-profile-isolation rationale, which is identical here.
#
# Credentials come from LK_AWS_* Buildkite secrets (the watercooler-livekit-ci
# IAM user — see ardiustech/watercooler's infrastructure/livekit/ci_user.tf).
# NOT the same user/creds as watercooler's own WC_AWS_* (different repo,
# different scope: this user can only push to the livekit-server ECR repo and
# SendCommand to the livekit instance, nothing else).
set -euo pipefail

# Soft-skip until CD is activated (LK_AWS_* secrets set), so a merge before
# activation is green rather than red. See .buildkite/README.md.
if [ -z "${LK_AWS_ACCESS_KEY_ID:-}" ] || [ -z "${LK_AWS_SECRET_ACCESS_KEY:-}" ]; then
  echo "CD not activated: LK_AWS_ACCESS_KEY_ID / LK_AWS_SECRET_ACCESS_KEY not set."
  echo "Skipping deploy. See .buildkite/README.md to activate."
  exit 0
fi

ACCOUNT="${DEPLOY_ACCOUNT_ID:-396735084811}"
REGION="${LK_AWS_REGION:-${LK_AWS_DEFAULT_REGION:-us-west-2}}"
REPO="${ECR_REPO:-livekit-server}"
INSTANCE_TAG="${INSTANCE_NAME_TAG:-watercooler-livekit-prod}"
REGISTRY="$ACCOUNT.dkr.ecr.$REGION.amazonaws.com"
IMAGE="$REGISTRY/$REPO"
TAG="${BUILDKITE_COMMIT:-latest}"
# How long to let LiveKit finish draining active calls on the box before the
# SSM command itself gives up waiting (the on-box deploy.sh has its own,
# separate DRAIN_TIMEOUT bound on the docker-level stop — this is just how
# long THIS step polls for that to finish). Generous by design; see
# infrastructure/livekit/README.md in ardiustech/watercooler for the full
# graceful-drain rationale (LiveKit's own SIGTERM handling, not a new
# mechanism this pipeline invents).
POLL_ATTEMPTS="${DEPLOY_POLL_ATTEMPTS:-200}" # 200 * 5s = ~16.5 min ceiling

# Isolated AWS profile dir (never the agent's ~/.aws), cleaned up on exit.
AWS_DIR="$(mktemp -d)"
trap 'rm -rf "$AWS_DIR"' EXIT
cat >"$AWS_DIR/credentials" <<CREDS
[lk]
aws_access_key_id=$LK_AWS_ACCESS_KEY_ID
aws_secret_access_key=$LK_AWS_SECRET_ACCESS_KEY
CREDS
cat >"$AWS_DIR/config" <<CONF
[profile lk]
region=$REGION
CONF

# Run aws-cli in a container with the "lk" profile mounted, so the agent needn't
# have the CLI and its own AWS env stays untouched.
awscli() {
  docker run --rm \
    -v "$AWS_DIR":/root/.aws:ro \
    -e AWS_PROFILE=lk -e AWS_DEFAULT_REGION="$REGION" \
    amazon/aws-cli:latest "$@"
}

echo "--- :docker: buildx builder"
docker buildx inspect lkbuilder >/dev/null 2>&1 \
  || docker buildx create --name lkbuilder --driver docker-container
docker buildx use lkbuilder

echo "--- :ecr: login (lk profile)"
awscli ecr get-login-password | docker login --username AWS --password-stdin "$REGISTRY"

echo "--- :hammer: build + push ($TAG)"
# amd64 only — the LiveKit box is a c6i (Intel) instance, not arm64 like the
# app's t4g box (see ardiustech/watercooler's infrastructure/livekit/README.md
# "Notes / decisions": LiveKit is CPU-bound on media forwarding, x86_64 is the
# well-trodden path). No emulation/binfmt needed since the agent is already
# amd64.
docker buildx build --platform linux/amd64 \
  -t "$IMAGE:$TAG" -t "$IMAGE:latest" --push .

echo "--- :rocket: trigger graceful-drain deploy via SSM"
CMD_ID="$(awscli ssm send-command \
  --document-name AWS-RunShellScript \
  --targets "Key=tag:Name,Values=$INSTANCE_TAG" \
  --comment "livekit-server CD $TAG" \
  --parameters "commands=[\"/opt/livekit/deploy.sh $TAG\"]" \
  --query 'Command.CommandId' --output text)"
echo "command id: $CMD_ID"

for _ in $(seq 1 "$POLL_ATTEMPTS"); do
  sleep 5
  STATUS="$(awscli ssm list-command-invocations --command-id "$CMD_ID" \
    --query 'CommandInvocations[0].Status' --output text 2>/dev/null || echo Pending)"
  echo "  deploy status: $STATUS"
  case "$STATUS" in
    Success) echo "+++ :white_check_mark: deploy succeeded"; exit 0 ;;
    Failed | Cancelled | TimedOut)
      echo "+++ :x: deploy $STATUS — on-box output:" >&2
      awscli ssm list-command-invocations --command-id "$CMD_ID" --details \
        --query 'CommandInvocations[0].CommandPlugins[0].Output' --output text >&2 || true
      exit 1
      ;;
  esac
done
echo "timed out waiting for the on-box deploy (it may still complete — the box's own DRAIN_TIMEOUT can legitimately run longer than this poll; check SSM directly: aws ssm list-command-invocations --command-id $CMD_ID)" >&2
exit 1
