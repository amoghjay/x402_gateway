# Builds any binary under cmd/: docker build --build-arg BINARY=x402-filter .
FROM golang:1.27 AS build
ARG BINARY
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath \
	-ldflags "-s -w -X github.com/amoghjay/x402_gateway/internal/buildinfo.Version=${VERSION}" \
	-o /out/app ./cmd/${BINARY}

# Distroless static: no shell, no package manager, uid 65532. Kubernetes runAsNonRoot
# accepts it as-is.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/app /app
ENTRYPOINT ["/app"]
