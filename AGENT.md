# AGENT.md

This file provides guidance for AI coding agents working in the **crema** repository (`github.com/kubebottle/crema`).

## Git Commit Guidelines

All commits MUST follow the **Angular Commit Message Guidelines**.

### Commit Message Format

```text
<type>(<scope>): <subject>
<BLANK LINE>
<body>
<BLANK LINE>
<footer>
```

The header (`<type>(<scope>): <subject>`) is mandatory; `<scope>`, `<body>`, and `<footer>` are optional. Keep the header line under 100 characters (preferably 50–72 characters).

### Type

Must be one of the following:

- **feat**: A new feature
- **fix**: A bug fix
- **docs**: Documentation only changes
- **style**: Changes that do not affect the meaning of the code (white-space, formatting, missing semi-colons, etc.)
- **refactor**: A code change that neither fixes a bug nor adds a feature
- **perf**: A code change that improves performance
- **test**: Adding missing tests or correcting existing tests
- **build**: Changes that affect the build system or external dependencies (e.g., `go.mod`, `Makefile`, `Dockerfile`)
- **ci**: Changes to CI configuration files and scripts (e.g., `.github/workflows`)
- **chore**: Other changes that don't modify `src` or `test` files
- **revert**: Reverts a previous commit

### Scope

Optional noun describing the section of the codebase affected, e.g.:
- `api`, `controller`, `agent`, `snapshot`, `crd`, `docs`, `deps`, `ci`

### Subject

- Use the imperative, present tense: "add", "change", "fix" (not "added", "changed", "fixes").
- Do not capitalize the first letter.
- No dot (`.`) at the end.

### Body

- Use the imperative, present tense.
- Explain the **motivation** for the change and contrast it with previous behavior.

### Footer

- **Breaking Changes**: Start with `BREAKING CHANGE:` followed by a description of the change, justification, and migration notes.
- **Referencing Issues**: e.g., `Closes #123`, `Fixes #456`.

### Examples

```text
feat(controller): add GPUWorkspace pause and resume reconciliation
```

```text
fix(api): validate non-negative GPU count in GPUWorkspaceSpec

Reject negative GPU counts during admission/validation to prevent
invalid Pod resource requests.

Closes #42
```

```text
docs: add architecture design doc and README diagram
```

## Project Overview

**crema** is a Kubernetes operator (scaffolded with Kubebuilder v4, Go 1.23) for declarative GPU workspace management (`GPUWorkspace` CRD under `crema.kubebottle.io/v1alpha1`).

- See [README.md](README.md) for the high-level overview.
- See [docs/design.md](docs/design.md) for the full architecture, CRD specification, and state machine details.

### Key Paths

- `api/v1alpha1/` — `GPUWorkspace` CRD API types
- `cmd/` — Operator entrypoint
- `internal/` — Controller and internal packages
- `config/` — Kubernetes manifests, RBAC, CRD, and kustomize configs
- `Makefile` — Build, codegen (`manifests`, `generate`), lint, and test targets
