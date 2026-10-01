# syntax=docker/dockerfile:1

FROM node:26-alpine AS ui-builder

WORKDIR /src/web

# Cache npm install separately from source changes.
COPY web/package.json web/package-lock.json ./
RUN npm ci

COPY web/ ./
# Builds into ../internal/api/ui/dist per web/vite.config.ts build.outDir.
RUN npm run build

FROM golang:1.27-alpine AS builder

ARG VERSION=dev

WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Overlay the built React console into the package that go:embed's from.
# Replaces the dist/.gitkeep placeholder so ui.Handler() serves the app
# instead of a 503 UI-not-built response.
COPY --from=ui-builder /src/internal/api/ui/dist ./internal/api/ui/dist

RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/gateway \
    ./cmd/gateway

# Same check/generation `make notices` runs (see Makefile, tools/notices) --
# invoked directly rather than via `make` since golang:1.27-alpine doesn't
# have it installed. Fails the image build if a Go dependency's license
# isn't on tools/notices/main.go's allow list.
RUN go run -C tools/notices . -repo-root ../.. -pkg ./cmd/gateway -out /out/THIRD_PARTY_NOTICES

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/gateway /gateway

# Third-party license notices for everything bundled in this image: the Go
# dependencies + standard library (THIRD_PARTY_NOTICES, generated above --
# see tools/notices), and the gateway's own license (LICENSE, NOTICE). The
# embedded React console's own dependency notices are already inside the
# binary and served at /licenses.txt by the running gateway -- see
# web/vite.config.ts's rollup-plugin-license and internal/api/ui/ui.go --
# not duplicated as a file here.
COPY --from=builder /out/THIRD_PARTY_NOTICES /licenses/THIRD_PARTY_NOTICES
COPY LICENSE NOTICE /licenses/

EXPOSE 8080 8081 8082

ENTRYPOINT ["/gateway"]
