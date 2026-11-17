FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/core ./cmd/core \
 && mkdir -p /out/data/audio

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/core /core
# Pre-create the audio dir owned by the nonroot user so the named volume
# inherits writable ownership on first mount.
COPY --from=build --chown=65532:65532 /out/data /data
EXPOSE 8080
ENTRYPOINT ["/core"]
