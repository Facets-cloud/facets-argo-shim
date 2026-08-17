FROM golang:1.25-alpine AS build
WORKDIR /src
COPY resolver/go.mod resolver/go.sum ./
RUN go mod download
COPY resolver/ .
RUN CGO_ENABLED=0 go build -o /out/facets-resolver .

FROM alpine:3.20
# ca-certificates: facets-resolver talks to the Facets control plane over
# HTTPS. No helm, no kustomize, no plugin.yaml, no cmp-server entrypoint —
# this image only ships the shim itself, for an initContainer to `cp` (along
# with the real helm binary, provided separately) into argocd-repo-server's
# shared /custom-tools emptyDir. See README.md for the full repo-server
# wiring.
#
# Deliberately NOT baked at /custom-tools: that path is also where the
# initContainer mounts the shared emptyDir it copies these files INTO — if
# they were baked there directly, the volume mount would shadow (hide) them
# before the `cp` step ever ran, and the copy would silently fail to find its
# source. /opt/facets is a neutral, never-mounted-over image path that only
# ever holds the pre-copy originals.
RUN apk add --no-cache ca-certificates
COPY --from=build /out/facets-resolver /opt/facets/facets-resolver
COPY helm-shim.sh /opt/facets/helm-shim.sh
# RUN chmod, not COPY --chmod: the COS docker used for remote builds
# (hack/build-remote.sh) predates BuildKit and doesn't support --chmod.
RUN chmod 0755 /opt/facets/facets-resolver /opt/facets/helm-shim.sh
