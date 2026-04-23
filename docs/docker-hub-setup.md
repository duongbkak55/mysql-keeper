# Publishing mysql-keeper to Docker Hub

Every push to `main` and every `v*` tag triggers
[.github/workflows/docker-publish.yml](../.github/workflows/docker-publish.yml),
which builds the controller image for `linux/amd64` + `linux/arm64` and
pushes it to Docker Hub. This doc walks through the one-time setup.

## One-time Docker Hub setup

1. **Sign up** at https://hub.docker.com if you do not already have an account.
   The workflow defaults to the repository name `mysql-keeper` under your
   username.
2. **Create the repository** at https://hub.docker.com/repository/create
   (optional — it will be auto-created on first push if your token has write
   scope, but creating it up front lets you set a description + README).
   - Namespace: your username (e.g. `duongbkak55`)
   - Repository name: `mysql-keeper`
   - Short description: "K8s controller for DC-DR PXC switchover"
   - Visibility: Public (recommended) or Private
3. **Create a Personal Access Token** at
   https://app.docker.com/settings/personal-access-tokens:
   - Name: `github-actions-mysql-keeper`
   - Access permissions: "Read, Write, Delete"
   - Copy the token — Docker Hub only shows it once.

## GitHub secrets

In the mysql-keeper repository on GitHub, go to
**Settings → Secrets and variables → Actions** and add:

| Secret | Value |
|--------|-------|
| `DOCKERHUB_USERNAME` | your Docker Hub username (e.g. `duongbkak55`) |
| `DOCKERHUB_TOKEN`    | the Personal Access Token from step 3 |

Optional override:

| Variable | Value |
|----------|-------|
| `DOCKERHUB_IMAGE` | Full image name. Defaults to `${DOCKERHUB_USERNAME}/mysql-keeper`. Set it if you want a different name or an org-scoped repo (e.g. `acme/mysql-keeper`). |

## What gets published

| Git event | Tags pushed |
|-----------|-------------|
| Push to `main` | `:latest`, `:main`, `:sha-<12-char-sha>` |
| Push tag `v0.2.0` | `:0.2.0`, `:0.2`, `:sha-<sha>` (plus the main-branch tags if the tag is on main) |
| Manual run with `extra_tag=rc-1` | `:rc-1`, `:sha-<sha>` |

Every image carries OCI labels so Docker Hub renders the source repo,
commit SHA, and license on the repo page:

```
org.opencontainers.image.source=https://github.com/<owner>/mysql-keeper
org.opencontainers.image.revision=<git-sha>
org.opencontainers.image.title=mysql-keeper
```

## First publish — manual validation

1. Push a commit to `main` (or click **Run workflow** on the
   `docker-publish` workflow under the Actions tab).
2. Wait for the job to finish (~4 min for a cold cache, ~1 min warm).
3. Verify on Docker Hub:
   ```bash
   docker pull duongbkak55/mysql-keeper:latest
   docker inspect duongbkak55/mysql-keeper:latest --format '{{.Os}}/{{.Architecture}}'
   # linux/amd64 on x86 host, linux/arm64 on Apple Silicon
   ```

## Using the published image in your deployment

The sample CSPs in `config/samples/` and `deploy/dc/kustomization.yaml`
still reference `ghcr.io/duongnguyen/mysql-keeper`. Point them at your Docker
Hub image:

```yaml
# deploy/dc/kustomization.yaml
images:
  - name: ghcr.io/duongnguyen/mysql-keeper
    newName: duongbkak55/mysql-keeper
    newTag: "0.2.0"   # pin to a semantic tag in production — avoid :latest
```

Or pass it to `scripts/e2e-setup.sh` via `IMAGE=duongbkak55/mysql-keeper:latest`
for ad-hoc testing against the published image.

## Syncing the repository description (optional)

The workflow tries to upload `docs/dockerhub-overview.md` as the Docker Hub
repo README on every push to `main`. That step calls
`PATCH /v2/repositories/{namespace}/{repo}`, which Docker Hub grants **only
to account-level auth**:

- Your account **password** (not recommended in CI), or
- A **Personal Access Token with the account-admin scope**, which is a
  separate scope from the image push/pull scope.

A standard "Read, Write, Delete" PAT (the one used for image push) returns
`403 Forbidden` on this endpoint. The workflow marks the description step
`continue-on-error: true` so the image still publishes — only the auto-
README sync is skipped.

Two ways to enable it:

**Option A — manual paste (simplest, no extra scope):**

1. Copy the contents of
   [`docs/dockerhub-overview.md`](./dockerhub-overview.md).
2. Paste into the "Repository Overview" field at
   `https://hub.docker.com/r/<namespace>/mysql-keeper/~/edit/`.
3. Re-do on every material change to the overview — rare.

**Option B — dedicated description token:**

1. On Docker Hub, create a second PAT named `github-description-sync`
   with **"Admin" scope** (granular scope "Read & Write" → "Accounts").
2. Add it as a GitHub secret called `DOCKERHUB_DESCRIPTION_TOKEN`
   (the workflow already falls back to it before `DOCKERHUB_TOKEN`).
3. Re-run the workflow — the step should now succeed.

## Troubleshooting

- **`UNAUTHORIZED: authentication required`** — the PAT expired or was
  regenerated. Recreate it on Docker Hub and update the
  `DOCKERHUB_TOKEN` secret.
- **Image pushes but tag list looks empty on Docker Hub** — make sure the
  repo was created under the right namespace; private repos need you to be
  logged in to Docker Hub to view them.
- **`no matching manifest for linux/arm64` when pulling on Apple Silicon**
  — the build only published amd64. Check the workflow log for the `buildx`
  step and confirm `platforms: linux/amd64,linux/arm64` appeared in the
  invocation.
- **Job `skipped`** — the workflow has an `if: github.repository == ...`
  guard to avoid publishing from forks. If you renamed the repo or fork,
  update the condition in `.github/workflows/docker-publish.yml`.

## Cost

Docker Hub's free tier covers unlimited public repositories and 1 private
repository, with image pull rate limits that apply to anonymous / free-tier
consumers only. Authenticated pulls from your own K8s cluster (via
`imagePullSecrets`) are not affected. For the scale of a DC-DR operator
(two clusters pulling occasionally), the free tier is plenty.
