# Build: docker build -t camfarm .
# Run:   docker run --rm -p 127.0.0.1:8080:8080 -p 127.0.0.1:8554:8554 -p 127.0.0.1:8000:8000 camfarm
#
# CamFarm is pure Go (no cgo), so the binary is static and the runtime image
# holds nothing else. See docs/docker.md for ports, advertised addresses and
# why multicast discovery is not offered here.

ARG GO_VERSION=1.26
FROM golang:${GO_VERSION} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Stamped by the release workflow; a plain `docker build` reports "dev".
ARG VERSION=
ARG COMMIT=
ARG DATE=
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w \
    -X github.com/0x524a/camfarm/internal/buildinfo.Version=${VERSION} \
    -X github.com/0x524a/camfarm/internal/buildinfo.Commit=${COMMIT} \
    -X github.com/0x524a/camfarm/internal/buildinfo.Date=${DATE}" \
    -o /out/camfarm ./cmd/camfarm

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/camfarm /camfarm
USER nonroot:nonroot

# The dashboard (8080), RTSP (8554) and ONVIF (8000) listen on fixed ports so
# they can be published. The dashboard has no authentication: publish it on
# 127.0.0.1 only, as in the run line above.
EXPOSE 8080 8554 8000

# --advertise-host is what clients are told to dial. 127.0.0.1 suits a client on
# the machine that runs the container with the ports published 1:1; override it
# for any other client (docs/docker.md).
ENTRYPOINT ["/camfarm"]
CMD ["--cameras", "3", \
     "--http-addr", "0.0.0.0:8080", \
     "--listen-host", "0.0.0.0", \
     "--rtsp-port", "8554", \
     "--onvif-port", "8000", \
     "--advertise-host", "127.0.0.1"]
