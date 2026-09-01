# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG SERVICE=llm-gateway
ARG SOURCE_REVISION=development
RUN case "$SERVICE" in \
      llm-gateway|control-plane|metering|bff|schema-migrate|provider-bootstrap|gateway-bootstrap) ;; \
      *) echo "unsupported SERVICE: $SERVICE" >&2; exit 2 ;; \
    esac && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.buildSHA=$SOURCE_REVISION" -o /out/service "./cmd/$SERVICE"

FROM node:22-alpine AS web-build
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM postgres:18-alpine@sha256:d3e1620b530c944afa6e887d22eb899824da68e19c52024bf98f5220c88a65b2 AS role-config
COPY scripts/postgres/configure-tenant-admin-roles.sql /opt/llm-gateway/configure-tenant-admin-roles.sql
COPY scripts/postgres/configure-metering-role.sql /opt/llm-gateway/configure-metering-role.sql
COPY deploy/gcp/configure-runtime-roles.sh /usr/local/bin/configure-runtime-roles
RUN chmod 0555 /usr/local/bin/configure-runtime-roles
ENTRYPOINT ["/usr/local/bin/configure-runtime-roles"]

FROM gcr.io/distroless/static-debian12:nonroot AS service
COPY --from=build /out/service /service
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/service"]

FROM gcr.io/distroless/static-debian12:nonroot AS bff
COPY --from=build /out/service /service
COPY --from=web-build /src/web/dist /web/dist
ENV BFF_WEB_DIST=/web/dist
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/service"]
