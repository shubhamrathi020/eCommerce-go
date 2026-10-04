# Go API. Build from the repository root:
#   docker build -t shop-api-go .
# The server needs the same PostgreSQL schema the Nest API (eCommerce-api) migrates; see README.md.

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

# A static binary needs no shell, libc or package manager: distroless "static" is a few megabytes and runs as non-root.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
ENV PORT=3333
COPY --from=build /out/api /api
EXPOSE 3333
HEALTHCHECK --interval=15s --timeout=3s --start-period=10s --retries=3 CMD ["/api", "-healthcheck"]
ENTRYPOINT ["/api"]
