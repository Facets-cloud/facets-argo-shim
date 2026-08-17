# facets-argo-shim

A drop-in replacement for the `helm` binary inside `argocd-repo-server`.
Standard Argo CD Applications using Argo's builtin Helm source — including
multi-source apps with a separate `$values` valueFiles ref — never go
through a Config Management Plugin, so there's no other render-time hook
point. This shim intercepts `helm template`'s own stdout and pipes it
through `facets-resolver`, a small Go binary that resolves every
`${facets:...}` reference against a Facets control plane before Argo CD ever
sees the rendered manifests. Every other `helm` invocation (`version`,
`dependency`, `pull`, ...) passes straight through untouched.

No Application-spec changes are required: refs can appear anywhere in any
chart's values files or templates, and resolution runs on the rendered
manifest stream regardless of how the chart produced them.

## Layout

    helm-shim.sh   the wrapper that shadows helm in argocd-repo-server
    Dockerfile     ships the shim + the static resolver binary
    resolver/      the Go resolver (facets-resolver): scan, identify, substitute

## Ref syntax and typing rules

Two ref forms are recognized: resource-output refs and blueprint-scoped
refs. Both share the same typing/escaping rules below.

### Resource-output refs

    ${facets:<type>.<name>.out.<path...>}

- `<type>.<name>` identifies a Facets resource (e.g. `sqs.orders`).
- `<path...>` walks its `out` block, e.g. `attributes.queue_url` or
  `interfaces.reader.connection_string`.

### Blueprint-scoped refs

    ${facets:blueprint.self.variables.<NAME>}
    ${facets:blueprint.self.secrets.<NAME>}
    ${facets:blueprint.self.artifacts.<NAME>}

`self` always means the same project/environment every other ref in the
render resolves against (per "Coordinate resolution" below) — there is no
cross-project or cross-environment blueprint ref.

- **`.variables.<NAME>`** — the project variable's effective value for the
  annotated environment. Referencing a variable that's actually a secret
  through this form is a hard error suggesting `.secrets.` instead — it
  never silently leaks a secret's presence or value through the wrong ref
  class.
- **`.secrets.<NAME>`** — the secret's value for the annotated environment.
  Referencing a non-secret through this form is a hard error suggesting
  `.variables.` instead. A secret that exists but has no value actually set
  for this environment (status other than `OVERRIDDEN`) is a hard error
  naming the secret and environment — never an empty string standing in for
  "unset".
  > **Prominent warning**: a `.secrets.` ref places the secret's actual
  > VALUE into the rendered manifest — the same manifest Argo CD stores,
  > diffs, and shows in its UI. Use `.secrets.` refs only inside manifests
  > of `kind: Secret` (where that visibility is already expected and
  > access-controlled the same way any other Kubernetes Secret is), or
  > prefer an external secret manager (e.g. External Secrets Operator, a
  > cloud provider's secret-injection webhook) that never puts the value in
  > a `helm template` render or an Argo CD diff at all. Do not use
  > `.secrets.` refs to populate a `ConfigMap`, an env var block on a
  > `Deployment`, or any other non-`Secret` manifest.
- **`.artifacts.<NAME>`** — the artifact's effective container image URI for
  the annotated environment (`<NAME>` is the artifact's CI integration
  name). Resolved by precedence: an `ENVIRONMENT`-registered URI for this
  exact environment, else a `RELEASE_STREAM`-registered URI for this
  environment's release stream, else a single unscoped default registration
  if one exists. No matching registration is a hard error listing every
  registration the artifact actually has.

### Typing, embedding, and escaping (both forms)

- Every path/name segment must be non-empty; a malformed ref is a hard error
  — nothing partially resolved is ever emitted.
- **Whole-scalar ref** (the entire YAML scalar is exactly one ref, e.g.
  `replicas: ${facets:service.api.out.attributes.replicas}`): the resolved
  value is injected with its native type (number, bool, string, or a nested
  object/list) and YAML anchors/comments on the original node are preserved.
- **Embedded ref** (a ref inside a larger string, e.g.
  `url: "https://${facets:svc.api.out.attributes.host}/path"`): the resolved
  value is stringified in place. A non-scalar (object/list) value used this
  way is an error.
- **Escape**: `$${facets:...}` resolves to the literal `${facets:...}`
  (only the doubled `$` is consumed) — for manifests that need to emit the
  placeholder syntax itself, e.g. as documentation or example text.

## Coordinate resolution

Every render needs a Facets project + environment to resolve refs against.
**Per-Application annotations are the only source — there is no
repo-server-wide fallback of any kind.** A live, lazy Kubernetes LIST — not
a shared file, not a separate daemon: the shim forwards the `--namespace`
and `--name-template` values it extracts from real helm's own argv to
`facets-resolver`. When the rendered stream contains any `${facets:...}`
ref, both flags are required; facets-resolver then builds an in-cluster kube
client (only at this point — never on a render with no refs at all) and
lists `Application` CRs in `FACETS_ARGOCD_NAMESPACE` (default `argocd`),
looking for exactly one whose effective destination namespace + release name
match. Release name precedence: `spec.source.helm.releaseName`
(single-source) → first non-empty `spec.sources[].helm.releaseName`
(multi-source) → `metadata.name`. The matched Application must carry both
`facets.cloud/project` and `facets.cloud/environment` annotations.

Every failure mode is a hard, fail-closed error — nothing partially
resolved, and nothing resolved against a guessed identity, ever reaches
stdout:

- either `--namespace`/`--name-template` is empty (refs are present, so
  there's no Application to identify);
- the Kubernetes LIST call itself errors;
- zero Applications match the destination namespace + release name;
- the single match is missing `facets.cloud/project`,
  `facets.cloud/environment`, or both (the error names the Application and
  exactly which key(s) are missing);
- more than one Application matches (**ambiguous** — the error names every
  matching Application).

If a rendered stream has zero `${facets:...}` refs, none of the above runs
at all — no kube client, no CP call, no flag is even read. The stream is
only passed through the `$$`-unescape step.

## Security posture

Per-Application coordinate resolution needs `argocd-repo-server`'s own
ServiceAccount token, mounted in-cluster, to LIST `Application` CRs — that's
a deliberate, scoped grant, worth stating plainly rather than leaving
implicit.

**The exact Role granted** (see the install recipes above for the full
Role/RoleBinding): `list` only, on `applications.argoproj.io`, namespaced to
Argo CD's own namespace (`argocd` by default, or `FACETS_ARGOCD_NAMESPACE`).
No `get`, no `watch`, no `create`/`update`/`delete`, no other resource type,
no other namespace, no cluster-wide grant.

**Why the token is needed at all**: this is the only way to identify which
Facets project/environment a given `helm template` render belongs to (see
"Coordinate resolution" above) — the shim has no other channel into Argo
CD's own state, and per-Application annotations are the only coordinate
source (there is no env-var fallback to fall back to instead).

**Residual risk, stated plainly**: `argocd-repo-server` already has broad
reach by design (it clones and renders every configured Git repo). This
grant adds read-only visibility into one more thing: every `Application`
object's spec and annotations (names, destinations, source repos/paths,
Facets project/environment labels) in the Argo CD namespace — not their
`Secret`s, not other namespaces, not write access to anything. If
`argocd-repo-server` is compromised, that's what this specific grant adds to
the blast radius. Upstream Argo CD manifests ship `repo-server` with
`automountServiceAccountToken: false` deliberately, precisely because it
normally needs no Kubernetes API access at all — enabling it here is a
conscious trade, not an oversight, and is why this section exists.

**Degraded mode without the token**: skip the RBAC and
`automountServiceAccountToken: true` entirely, and the shim still installs
and runs — any render whose manifests contain zero `${facets:...}` refs is
completely unaffected (see "Coordinate resolution": the kube client is never
even built on that path). Only renders that DO contain refs fail closed,
with an error explaining that per-Application coordinate resolution couldn't
run. This is a legitimate way to stage a rollout: install the shim first,
confirm it's a no-op for every existing Application, then grant the RBAC
once you're ready for `${facets:...}` refs to actually resolve.

**A separate, CP-side permission for `blueprint.self.secrets.*` refs**:
resolving a secret's actual value (not just proving it exists) requires the
`FACETS_CP_USERNAME`/`FACETS_CP_TOKEN` identity itself to hold Facets'
`VIEW_SECRETS` permission on the project. This is entirely independent of
the Kubernetes RBAC above — it's enforced by the Facets control plane, not
by anything in this repo — and is exactly the same permission `raptor get
variables --show-secrets` requires. Without it, any `.secrets.` ref fails
closed with the control plane's own authorization error; `.variables.` and
`.artifacts.` refs are unaffected.

## Install footprint

`argocd-repo-server` needs:

- An `initContainer`, using this image, that mounts the shared `/custom-tools`
  emptyDir (the *natural* path — it's the volume `argocd-repo-server` itself
  already exposes on `PATH` for custom tooling) and copies this image's
  baked artifacts into it:

      cp /opt/facets/facets-resolver /custom-tools/
      cp /opt/facets/helm-shim.sh /custom-tools/helm-shim
      cp /usr/local/bin/helm /custom-tools/helm-real   # real helm, from wherever it's sourced
      chmod +x /custom-tools/facets-resolver /custom-tools/helm-shim /custom-tools/helm-real
      ln -sf /custom-tools/helm-shim /custom-tools/helm

  Two path spaces, easy to conflate — keep them straight:
  - **Image paths** (`/opt/facets/...`): where this image bakes the shim and
    the resolver binary, at build time, before anything is mounted over
    them.
  - **Runtime paths** (`/custom-tools/...`): where the initContainer copies
    them TO, inside the shared emptyDir that both the initContainer and the
    `argocd-repo-server` container mount at `/custom-tools`. `helm-shim.sh`'s
    own defaults (`REAL_HELM=/custom-tools/helm-real`,
    `FACETS_RESOLVER_BIN=/custom-tools/facets-resolver`) are these
    POST-copy runtime paths — the script runs inside `argocd-repo-server`,
    never inside this image, so it only ever needs to know the runtime
    layout.
  - The image never bakes anything directly at `/custom-tools`: that's the
    initContainer's own mount point, so baking there would let the volume
    mount shadow the baked files before the `cp` step ever sees them. Copy
    FROM `/opt/facets` INTO `/custom-tools`, never the reverse.
- That shared `/custom-tools` `emptyDir` volume mounted into both the
  initContainer and the `argocd-repo-server` container, with
  `/custom-tools` first on `PATH` inside `argocd-repo-server` so `helm`
  resolves to the shim, not the real binary.
- `envFrom` (or explicit `env`) on `argocd-repo-server` providing
  `FACETS_CP_URL`, `FACETS_CP_USERNAME`, `FACETS_CP_TOKEN`.
- **Required, not optional**: RBAC (a Role/RoleBinding or
  ClusterRole/ClusterRoleBinding) granting `argocd-repo-server`'s
  ServiceAccount `list` on `applications.argoproj.io` in the Argo CD
  namespace, plus `automountServiceAccountToken: true` on the repo-server
  pod spec (upstream installs often set this `false`). There is no fallback
  coordinate source — any Application whose rendered manifests contain
  `${facets:...}` refs fails its render without this RBAC. Set
  `FACETS_ARGOCD_NAMESPACE` if Argo CD's own namespace isn't `argocd`.

`REAL_HELM` and `FACETS_RESOLVER_BIN` are overridable via env on the
`argocd-repo-server` container (defaults `/custom-tools/helm-real` and
`/custom-tools/facets-resolver` — the runtime paths above), primarily for
testing.


## Install — Helm chart (argo-helm)

Everything lives under `repoServer.*` values. This is the tested wiring: the
first initContainer copies **Argo's own helm** (version-exact by construction),
the second copies this image's artifacts, and a `subPath` mount shadows
`/usr/local/bin/helm` with the shim.

```yaml
repoServer:
  automountServiceAccountToken: true   # required: per-App annotations are the only coordinate source
  initContainers:
    - name: copy-helm-real
      image: quay.io/argoproj/argocd:v3.3.5        # match your Argo version
      command: [sh, -c, "cp /usr/local/bin/helm /custom-tools/helm-real"]
      volumeMounts:
        - {name: custom-tools, mountPath: /custom-tools}
    - name: copy-facets-tools
      image: docker.io/facetscloud/facets-argo-shim:v0.12
      command: [sh, -c, "cp /opt/facets/facets-resolver /custom-tools/ && cp /opt/facets/helm-shim.sh /custom-tools/helm-shim && chmod 755 /custom-tools/*"]
      volumeMounts:
        - {name: custom-tools, mountPath: /custom-tools}
  volumes:
    - {name: custom-tools, emptyDir: {}}
  volumeMounts:
    - {name: custom-tools, mountPath: /usr/local/bin/helm, subPath: helm-shim}
    - {name: custom-tools, mountPath: /custom-tools}
  envFrom:
    - secretRef: {name: facets-cp-credentials}

extraObjects:
  - apiVersion: v1
    kind: Secret
    metadata: {name: facets-cp-credentials, namespace: argocd}
    type: Opaque
    stringData:
      FACETS_CP_URL: https://<org>.console.facets.cloud
      FACETS_CP_USERNAME: <user>
      FACETS_CP_TOKEN: <token>
  - apiVersion: rbac.authorization.k8s.io/v1   # required: per-App annotations are the only coordinate source
    kind: Role
    metadata: {name: facets-shim-app-reader, namespace: argocd}
    rules:
      - {apiGroups: [argoproj.io], resources: [applications], verbs: [list]}
  - apiVersion: rbac.authorization.k8s.io/v1
    kind: RoleBinding
    metadata: {name: facets-shim-app-reader, namespace: argocd}
    roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: facets-shim-app-reader}
    subjects:
      - {kind: ServiceAccount, name: argocd-repo-server, namespace: argocd}
```

Note: on the argo-helm chart the repo-server ServiceAccount is usually named
`<release>-argocd-repo-server` — adjust the RoleBinding subject.

## Install — kubectl (existing installs)

```bash
# 1. credentials
kubectl -n argocd create secret generic facets-cp-credentials \
  --from-literal=FACETS_CP_URL=https://<org>.console.facets.cloud \
  --from-literal=FACETS_CP_USERNAME=<user> \
  --from-literal=FACETS_CP_TOKEN=<token>

# 2. RBAC (required: per-App annotations are the only coordinate source)
kubectl -n argocd create role facets-shim-app-reader \
  --verb=list --resource=applications.argoproj.io
kubectl -n argocd create rolebinding facets-shim-app-reader \
  --role=facets-shim-app-reader --serviceaccount=argocd:argocd-repo-server

# 3. patch the repo-server
kubectl -n argocd patch deployment argocd-repo-server --patch-file shim-patch.yaml
```

`shim-patch.yaml` (strategic merge). **Pick your variant** — the repo-server
container's name depends on how Argo CD was installed, and a strategic-merge
patch only lands on a container name it matches exactly:

- **Upstream raw manifests** (`kubectl apply -f install.yaml`, what this
  recipe targets): container name `argocd-repo-server`, used below.
- **argo-helm chart installs**: container name `repo-server` instead — use
  the "Install — Helm chart" recipe above (it patches via chart values, not
  this kubectl patch, so the name difference doesn't come up there).

```yaml
spec:
  template:
    spec:
      automountServiceAccountToken: true   # upstream install.yaml sets false
      initContainers:
        - name: copy-helm-real
          image: quay.io/argoproj/argocd:v3.3.5
          command: [sh, -c, "cp /usr/local/bin/helm /custom-tools/helm-real"]
          volumeMounts:
            - {name: custom-tools, mountPath: /custom-tools}
        - name: copy-facets-tools
          image: docker.io/facetscloud/facets-argo-shim:v0.12
          command: [sh, -c, "cp /opt/facets/facets-resolver /custom-tools/ && cp /opt/facets/helm-shim.sh /custom-tools/helm-shim && chmod 755 /custom-tools/*"]
          volumeMounts:
            - {name: custom-tools, mountPath: /custom-tools}
      containers:
        - name: argocd-repo-server   # "repo-server" on argo-helm installs — see note above
          envFrom:
            - secretRef: {name: facets-cp-credentials}
          volumeMounts:
            - {name: custom-tools, mountPath: /usr/local/bin/helm, subPath: helm-shim}
            - {name: custom-tools, mountPath: /custom-tools}
      volumes:
        - {name: custom-tools, emptyDir: {}}
```

Uninstall = remove the patch/values and delete the Secret/RBAC; the
repo-server returns to stock. Apps are untouched either way — the shim adds
no fields to any Application.

## Limitations

- Only `helm template`'s stdout is intercepted. Refs anywhere else in the
  render pipeline (e.g. a plugin, a raw manifest source without Helm) are
  not resolved by this mechanism.
- Per-Application coordinate resolution requires the shim to see
  `--namespace`/`--name-template` on helm's own argv, which is populated by
  Argo CD's builtin Helm source invocation — not guaranteed by every
  possible repo-server configuration or Helm version. There is no fallback:
  if either flag is missing, any render whose manifests contain
  `${facets:...}` refs hard-fails.
- Per-Application annotations are the only coordinate source — there is no
  repo-server-wide default. A missing or misconfigured
  `facets.cloud/project`/`facets.cloud/environment` annotation on the
  matched Application fails that render loudly rather than resolving
  against a guessed identity.
- An Application without `spec.destination.namespace` set can never be
  matched (its effective destination namespace is empty, which never equals
  the shim's real `--namespace` argument) — any render of it containing
  `${facets:...}` refs fails closed with an error explaining the namespace +
  release name it looked for.
- Resolved values land in plaintext in the rendered manifests Argo CD
  applies, diffs, and shows in its UI — the same as any other Helm value.
  Resource-output refs and `blueprint.self.variables.*` refs are for plain
  configuration; route anything secret-shaped through either a
  `blueprint.self.secrets.*` ref placed only in a `kind: Secret` manifest
  (see the prominent warning under "Blueprint-scoped refs" above), or
  through an external secret manager instead — never through a
  resource-output or `.variables.` ref resolved into a `ConfigMap` or pod
  spec.
- No CMP mode, no kustomize support, no plain-directory support — helm-only,
  by design.
