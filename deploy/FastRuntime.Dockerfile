# syntax=docker/dockerfile:1.7
ARG BUILDER
ARG RUNTIME
FROM ${BUILDER} AS built
# Existing backend tests use repository fixtures and /usr/bin/true (Alpine has /bin/true).
COPY deploy/client-identity/ /app/deploy/client-identity/
RUN test -x /usr/bin/true || ln -s /bin/true /usr/bin/true
RUN --network=none \
    --mount=type=cache,id=sub2api-gomod,target=/go/pkg/mod \
    --mount=type=cache,id=sub2api-gobuild,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOMAXPROCS=4 go test -p 4 -tags=embed \
    ./internal/server ./internal/service ./internal/web ./cmd/server -count=1 -timeout=300s
FROM ${RUNTIME}
ARG COMMIT
ARG VERSION
COPY --from=built --chown=1000:1000 --chmod=755 /app/sub2api /app/sub2api
COPY --from=built --chown=1000:1000 /app/backend/resources/ /app/resources/
LABEL org.opencontainers.image.source="https://github.com/jzg-lab/sub2api-r3-fix" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.version="${VERSION}"
