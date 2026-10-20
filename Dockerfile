FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pintu . && mkdir -p /out/data

# Static binary only: no shell, no package manager. Runs as non-root UID 1000,
# which owns /data (also the owner used by images from the Node.js version).
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/pintu /pintu
COPY --from=build --chown=1000:1000 /out/data /data
USER 1000:1000

ENV DATA_DIR=/data PORT=8080
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s CMD ["/pintu", "healthcheck"]

ENTRYPOINT ["/pintu"]
