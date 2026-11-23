# Multi-stage build for the sc-component-observatory service.
#
# Shape follows sc-component-backup's, minus the database CLI tools:
# observatory shells out to nothing. It reads telemetry over cloud APIs
# and keeps its own state in object storage, so the runtime image is
# just the Go binary and CA certificates.
#
# Build args:
#   VERSION   auto-injected by SC on EVERY image build (CalVer
#             "YYYY.MM.DD-<short-sha>" in CI). A deployed binary
#             reporting 0.0.0 means this default leaked through and is
#             a bug — see root CLAUDE.md "How SC builds images".
#   COMMIT    NOT auto-injected. The deploy action exports GITHUB_SHA;
#             the release workflow passes it through.

ARG GOLANG_VERSION=1.26-alpine

# Plain amazonlinux (NOT lambda/provided) so the container's own
# ENTRYPOINT runs directly without lambda-entrypoint.sh demanding a
# handler argument. go-aws-lambda-sdk-style detection is not used here:
# the binary serves plain HTTP on $PORT and AWS Lambda's Function URL
# with RESPONSE_STREAM/BUFFERED fronts it.
#
# Why not the Lambda base image at all: three of its layers begin with
# a tar entry whose path is the empty string, which Yandex Cloud's
# registry rejects outright with "Bad path in image". Observatory is
# meant to deploy to YC too, so the base must be portable from day one
# rather than discovered to be a blocker at slice 6.
ARG RUNTIME_BASE=public.ecr.aws/amazonlinux/amazonlinux:2023

# -----------------------------
# Stage 1 — build the Go binary
# -----------------------------
FROM golang:${GOLANG_VERSION} AS go-builder

RUN apk add --no-cache ca-certificates git

WORKDIR /src

# Dependency layer first so a source-only change does not re-download
# the module cache. The AWS SDK tree is large and slow to fetch.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=0.0.0
ARG COMMIT=""
# CGO off for a static binary that runs on any glibc/musl base.
# -trimpath keeps build paths out of the binary; -s -w drop the symbol
# table and DWARF, which is a meaningful size win on a Lambda image.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/observatory \
      ./cmd/observatory

# -----------------------------
# Stage 2 — runtime
# -----------------------------
FROM ${RUNTIME_BASE}

# CA bundle is required: every outbound call this service makes is TLS
# (S3, CloudWatch, SecretsManager, YC gRPC).
RUN dnf install -y ca-certificates \
    && dnf clean all \
    && rm -rf /var/cache/dnf

COPY --from=go-builder /out/observatory /usr/local/bin/observatory

# Re-declare after FROM so the labels can see them.
ARG VERSION=0.0.0
ARG COMMIT=""
LABEL org.opencontainers.image.title="sc-component-observatory" \
      org.opencontainers.image.description="SC Component: deploy dashboard, logs and metrics across AWS / Yandex Cloud / Kubernetes" \
      org.opencontainers.image.source="https://github.com/simple-container-com/sc-component-observatory" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"

ENV PORT=8080
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/observatory"]
