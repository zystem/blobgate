# Governance

blobgate is a small project run by two maintainers. This file says how it is run, so that a contributor knows what to expect, a user knows what the controls are, and the project can go on if one of the two cannot.

## Members and roles

| Member | Role | Access to sensitive resources |
|---|---|---|
| `zystem` (the owner of the repository) | maintainer: decides, reviews and merges, makes releases, answers security reports, holds the settings of the repository (security settings, Dependabot, secret scanning), the container and chart packages and the badge entry | admin of the repository and its settings, the packages on ghcr.io, the badge entry on bestpractices.dev |
| `Juliesergienko` (collaborator, invited 2026-11-13) | second maintainer: reviews and merges changes, triages and closes issues, makes releases (a tag starts the release workflow) | write access to the repository (the most that a collaborator of a personal repository can have); editor of the badge entry |
| contributors | anyone who sends a pull request or an issue | none |

This is the complete list of members with access to sensitive resources; it is changed in this file when a permission is granted or removed.

## How the project is run

- **Decisions** are made by the maintainers, in the open: in issues and pull requests, with the reasons in the commit message or in `docs/`. When the two disagree, the owner decides.
- **Contributions** come as pull requests and follow [CONTRIBUTING.md](CONTRIBUTING.md). Every commit carries a sign-off ([DCO.md](DCO.md)); the `DCO` workflow checks it.
- **Escalated permissions** (write access to the repository, the right to publish releases or packages, access to the settings) are given only to a person who has contributed accepted changes over time, whose identity is
  known to the owner, and who uses two-factor authentication on GitHub. The owner grants and removes them, and says so here.
- **Releases** are made by the release workflow from a signed tag created by a maintainer ([docs/release.md](docs/release.md)); no person holds a signing key of the project: the files, the image and the chart are signed
  keyless by the workflow ([docs/verifying-releases.md](docs/verifying-releases.md)), and the tag is signed with the SSH key of the maintainer who makes it.
- **Review of changes**: the automatic gates check every change (tests with the real servers, coverage, `gofmt`, `staticcheck`, `gosec`, `govulncheck`, dependency review, DCO: [docs/security-policies.md](docs/security-policies.md)).
  The maintainers review each other's changes by convention; a maintainer may merge their own change when the gates pass and the other maintainer is away.
- **Security reports** go as in [SECURITY.md](SECURITY.md).

## If one maintainer cannot go on

The project must be able to create and close issues, accept changes and release a version within a week of the loss of either maintainer. What each one needs, and where it is:

| Task | What it needs | Who can do it |
|---|---|---|
| create and close issues, label and answer | write access | either |
| review and merge a change | write access; the checks pass | either |
| make a release | write access to push a tag; the steps in [docs/release.md](docs/release.md); the workflow `release.yml` signs the files with its own identity and publishes with the token of the run | either; the tag is signed with the key of whoever makes it |
| update the dependencies | Dependabot opens the pull requests; merge them | either |
| change the settings of the repository, the security settings, the packages, give or take access | admin | the owner only (a personal repository has no role between write and owner) |
| read private vulnerability reports | admin or security manager | the owner only; see [SECURITY.md](SECURITY.md) for what is done when the owner is away |

Nothing else is needed: the CI keeps no secret that a person could lose (the signing is keyless, the token of a run is made by GitHub for that run), the project has no domain name or server of its own, and the
licence (MIT) lets anyone who has the repository carry on. The keys and secrets of a deployment (the tenant secrets, the seal key, the key of the backups) are the operator's, never the project's.

If the **owner** is the one who cannot go on, the second maintainer carries on with write access as above. Taking over the settings, the packages and the badge entry needs the owner's account to be handed over or the
repository to be transferred (Settings, Transfer ownership); until then those three stay as they are and the releases go on.

## Bus factor

Two people can make a release and keep the project going; the project has one author of nearly all of the code so far. More contributors are welcome, and the second maintainer is chosen to share the work of review and
release, not only to stand in.
