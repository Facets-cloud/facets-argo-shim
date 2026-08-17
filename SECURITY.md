# Security Policy

## Supported versions

Only the latest published tag (`docker.io/facetscloud/facets-argo-shim:latest`,
matching the most recent `vX.Y.Z` release) receives security fixes. There is
no LTS branch — upgrade to the latest tag to pick up a fix.

## Reporting a vulnerability

Please report suspected vulnerabilities privately to **security@facets.cloud**
rather than opening a public issue. Include the version/tag in use, a
description of the issue, and reproduction steps if you have them. We'll
acknowledge receipt and follow up with a fix timeline.

## Threat model

This project intentionally grants `argocd-repo-server` a scoped, read-only
Kubernetes RBAC role, because coordinate resolution has no other channel
into Argo CD's own state. See the README's ["Security posture"](README.md#security-posture)
section for the exact Role, why it's needed, the residual risk if
`argocd-repo-server` is compromised, and how to run without it (degraded,
no-op for Applications with zero `${facets:...}` refs).
