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

    ${facets:<type>.<name>.out.<path...>}

- `<type>.<name>` identifies a Facets resource (e.g. `sqs.orders`).
- `<path...>` walks its `out` block, e.g. `attributes.queue_url` or
  `interfaces.reader.connection_string`.
- Every path segment must be non-empty; a malformed ref is a hard error —
  nothing partially resolved is ever emitted.
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
Resolved in order, per rendered stream:

1. **Per-Application**, via a live, lazy Kubernetes LIST — not a shared
   file, not a separate daemon. The shim forwards the `--namespace` and
   `--name-template` values it extracts from real helm's own argv to
   `facets-resolver`. If both are non-empty, facets-resolver builds an
   in-cluster kube client (only at this point — never on a render with no
   `${facets:...}` refs at all) and lists `Application` CRs in
   `FACETS_ARGOCD_NAMESPACE` (default `argocd`), looking for exactly one
   whose effective destination namespace + release name match. Release name
   precedence: `spec.source.helm.releaseName` (single-source) →  first
   non-empty `spec.sources[].helm.releaseName` (multi-source) →
   `metadata.name`. The matched Application must carry both
   `facets.cloud/project` and `facets.cloud/environment` annotations.
2. **Repo-server-wide fallback**: `FACETS_PROJECT`/`FACETS_ENVIRONMENT`,
   set once on the `argocd-repo-server` Deployment — one repo-server mapped
   to one Facets project+environment for every Application it renders that
   isn't (or can't be) resolved per-Application.

A per-Application lookup that runs but fails outright — a Kubernetes API
error, or an **ambiguous** match (more than one Application sharing the same
destination namespace + release name) — is a hard, fail-closed error: it
never silently falls through to the env-var fallback, since resolving
against the wrong environment would be worse than failing loudly. Only a
*clean* non-match (zero matching Applications, or a match lacking one or
both annotations) falls through to the env-var fallback.

If a rendered stream has zero `${facets:...}` refs, none of the above runs
at all — no kube client, no CP call, no env var read. The stream is only
passed through the `$$`-unescape step.

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
  `FACETS_CP_URL`, `FACETS_CP_USERNAME`, `FACETS_CP_TOKEN`, and
  `FACETS_PROJECT`/`FACETS_ENVIRONMENT` (repo-server-wide fallback
  coordinates).
- Optionally, for per-Application coordinates: RBAC (a Role/RoleBinding or
  ClusterRole/ClusterRoleBinding) granting `argocd-repo-server`'s
  ServiceAccount `list` on `applications.argoproj.io` in the Argo CD
  namespace, and `FACETS_ARGOCD_NAMESPACE` set if it isn't `argocd`. Without
  this, every render falls back straight to (2).

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
  automountServiceAccountToken: true   # needed for per-App annotations only
  initContainers:
    - name: copy-helm-real
      image: quay.io/argoproj/argocd:v3.3.5        # match your Argo version
      command: [sh, -c, "cp /usr/local/bin/helm /custom-tools/helm-real"]
      volumeMounts:
        - {name: custom-tools, mountPath: /custom-tools}
    - name: copy-facets-tools
      image: docker.io/facetscloud/facets-argo-shim:v0.9
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
      FACETS_PROJECT: <default-project>        # optional fallback
      FACETS_ENVIRONMENT: <default-environment>
  - apiVersion: rbac.authorization.k8s.io/v1   # per-App annotations only
    kind: Role
    metadata: {name: facets-shim-app-reader, namespace: argocd}
    rules:
      - {apiGroups: [argoproj.io], resources: [applications], verbs: [get, list]}
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
# 1. credentials (+ optional fallback coordinates)
kubectl -n argocd create secret generic facets-cp-credentials \
  --from-literal=FACETS_CP_URL=https://<org>.console.facets.cloud \
  --from-literal=FACETS_CP_USERNAME=<user> \
  --from-literal=FACETS_CP_TOKEN=<token> \
  --from-literal=FACETS_PROJECT=<default-project> \
  --from-literal=FACETS_ENVIRONMENT=<default-environment>

# 2. RBAC (per-App annotations only)
kubectl -n argocd create role facets-shim-app-reader \
  --verb=get,list --resource=applications.argoproj.io
kubectl -n argocd create rolebinding facets-shim-app-reader \
  --role=facets-shim-app-reader --serviceaccount=argocd:argocd-repo-server

# 3. patch the repo-server
kubectl -n argocd patch deployment argocd-repo-server --patch-file shim-patch.yaml
```

`shim-patch.yaml` (strategic merge; container name is `repo-server` on
upstream manifests):

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
          image: docker.io/facetscloud/facets-argo-shim:v0.9
          command: [sh, -c, "cp /opt/facets/facets-resolver /custom-tools/ && cp /opt/facets/helm-shim.sh /custom-tools/helm-shim && chmod 755 /custom-tools/*"]
          volumeMounts:
            - {name: custom-tools, mountPath: /custom-tools}
      containers:
        - name: repo-server
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
  possible repo-server configuration or Helm version.
- A single repo-server pod resolving refs for many Applications across
  different Facets projects/environments depends entirely on accurate
  per-Application annotations; a misconfigured or missing annotation falls
  back to the repo-server-wide default, which may be wrong for that
  Application.
- No CMP mode, no kustomize support, no plain-directory support — helm-only,
  by design.
