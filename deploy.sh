#!/usr/bin/env bash
# deploy.sh: deploys Labyrinth on the production VM, from the images CI pushed
# (.github/workflows/images.yml). Lives in /root/labyrinth beside the files it
# writes, and runs as root:
#
#   ./deploy.sh            # the newest prod build (the "prod" tag)
#   ./deploy.sh 3adee6a    # a given commit's build: a rollback, or a redeploy
#
# In order, stopping at the first failure with the running stack untouched:
#   1. Pulls backend:<tag> and reads which commit it was built from (its
#      org.opencontainers.image.revision label), so "prod" becomes a commit.
#   2. Downloads that commit's compose.prod.yaml from GitHub (the repository
#      is public), so the stack's layout always matches its images.
#   3. Rewrites .env from Secret Manager (every labyrinth-<NAME> secret below)
#      plus IMAGE_TAG, so a changed secret is picked up on the next deploy.
#   4. Checks the new compose.prod.yaml and .env together, then swaps them in;
#      the previous pair stays as *.prev.
#   5. Pulls the images, recreates what changed (docker compose up -d), and
#      waits for the backend to report healthy.
#
# Doesn't seed or migrate: the backend migrates itself on start, and seeding
# is a one-off (backend/README.md, "Deploying").
set -euo pipefail

registry=asia-south2-docker.pkg.dev/labyrinth-gdg-2026/labyrinth
raw=https://raw.githubusercontent.com/dscsnu/labyrinth-26
# The backend's and Caddy's settings, one labyrinth-<NAME> secret each.
# labyrinth-JWT_PUBLIC_KEY is the frontend's, so it isn't here.
secrets=(JWT_PRIVATE_KEY QR_SECRET GOOGLE_CLIENT_ID GOOGLE_CLIENT_SECRET ALLOWED_EMAIL_DOMAINS
	POSTGRES_PASSWORD FRONTEND_HOST BACKEND_HOST ACME_EMAIL)
# How long the new backend may take to report healthy: its healthcheck's
# start_period (30 s) plus a few checks.
health_wait=90

die() { echo "deploy: $*" >&2; exit 1; }
dc() { docker compose -f compose.prod.yaml "$@"; }

cd "$(dirname "$0")"
umask 077 # .env and its temporary copies: root only
trap 'rm -f compose.prod.yaml.new .env.new' EXIT # left over only by a failure

# 1. The commit to run.
want=${1:-prod}
echo "deploy: pulling backend:$want"
docker pull -q "$registry/backend:$want" >/dev/null || die "no image backend:$want"
rev=$(docker inspect -f '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$registry/backend:$want")
[[ $rev =~ ^[0-9a-f]{40}$ ]] || die "backend:$want has no commit label (not built by CI?)"
tag=${rev:0:7}
[[ $want == "$tag" ]] || echo "deploy: $want is commit $tag"

# 2. That commit's compose file.
curl -fsSL "$raw/$rev/compose.prod.yaml" -o compose.prod.yaml.new || die "can't download compose.prod.yaml for $tag"

# 3. A fresh .env. A value that's empty or spans lines would break compose's
# parsing, so either stops the deploy.
for s in "${secrets[@]}"; do
	v=$(gcloud secrets versions access latest --secret="labyrinth-$s") || die "can't read secret labyrinth-$s"
	[[ -n $v && $v != *$'\n'* ]] || die "secret labyrinth-$s is empty or spans lines"
	printf '%s=%s\n' "$s" "$v"
done >.env.new
echo "IMAGE_TAG=$tag" >>.env.new

# 4. Swap them in, keeping the previous pair, and back out if compose rejects
# the result (a missing value, a bad file).
for f in compose.prod.yaml .env; do
	[[ -e $f ]] && cp -p "$f" "$f.prev"
	mv "$f.new" "$f"
done
if ! dc config --quiet; then
	for f in compose.prod.yaml .env; do [[ -e $f.prev ]] && mv "$f.prev" "$f"; done
	die "compose rejected the new files; the previous ones are back"
fi

# 5. Deploy and wait for the backend.
dc pull --quiet
dc up -d --remove-orphans
echo "deploy: waiting up to ${health_wait}s for the backend"
for ((i = 0; i < health_wait; i += 3)); do
	status=$(docker inspect -f '{{.State.Health.Status}}' "$(dc ps -q backend)" 2>/dev/null || true)
	[[ $status == healthy ]] && break
	sleep 3
done
dc ps
[[ $status == healthy ]] || die "backend not healthy after ${health_wait}s (status: ${status:-none}); see: docker compose -f compose.prod.yaml logs backend"

# Images more than a week old that no container uses: older builds, kept a
# week so a rollback needn't pull.
docker image prune -af --filter until=168h >/dev/null
echo "deploy: $tag is live"
