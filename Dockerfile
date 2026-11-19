FROM node:22-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.26 AS build
# The release version, shown by GET /api/v1/version (the release workflow passes the tag).
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/getplinth/plinth/internal/api.Version=${VERSION}" -o /plinth ./cmd/plinth \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /devidp ./cmd/devidp

# Development identity provider (no passwords!) for tests on throwaway clusters.
# Not part of the default image: build it with `docker build --target devidp`.
FROM alpine:3.21 AS devidp
RUN adduser -D -u 10001 devidp
COPY --from=build /devidp /devidp
USER devidp
ENTRYPOINT ["/devidp"]

# git is required by drivers that populate repositories from a template.
FROM alpine:3.21
ARG VERSION=dev
LABEL org.opencontainers.image.title="Plinth" \
      org.opencontainers.image.description="Self-service project onboarding: repository, registry project, namespace and database on request" \
      org.opencontainers.image.source="https://github.com/getplinth/plinth" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"
RUN apk add --no-cache git ca-certificates && adduser -D -u 10001 plinth
COPY --from=build /plinth /plinth
# The licence and the notices of what is inside (the web interface bundles fonts under the SIL OFL).
COPY LICENSE NOTICE THIRD_PARTY_NOTICES.md /licenses/
COPY third_party /licenses/third_party
USER plinth
EXPOSE 8080
ENTRYPOINT ["/plinth"]
