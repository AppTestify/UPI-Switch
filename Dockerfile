# Builds every binary (switch, pspsim, keygen, demo, seed, traffic) into one small non-root image.
FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/... && mkdir -p /out/keys

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/switch /out/pspsim /out/keygen /out/demo /out/seed /out/traffic /
COPY --from=build --chown=65532:65532 /out/keys /keys
COPY deploy/psps.json /etc/upi/psps.json
USER nonroot
EXPOSE 8080
ENTRYPOINT ["/switch"]
