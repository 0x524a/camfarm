# Running CamFarm in a container

CamFarm is pure Go with no cgo, so the image is a static binary on a distroless base. Nothing
else is installed.

```sh
docker build -t camfarm .
docker run --rm \
  -p 127.0.0.1:8080:8080 -p 127.0.0.1:8554:8554 -p 127.0.0.1:8000:8000 \
  camfarm
```

Open `http://127.0.0.1:8080/`. The cameras are at `rtsp://127.0.0.1:8554/cam-1` and so on. The
same commands work with `nerdctl` in place of `docker`.

## Ports and the advertised address

A fleet inside a container binds `0.0.0.0` so published ports can reach it, but a client must be
told an address it can dial. `--advertise-host` is that address: it goes into every RTSP URL,
ONVIF endpoint, snapshot URI and discovery announcement. The image defaults it to `127.0.0.1`,
which suits a client on the machine running the container.

- **Publish each port 1:1** (`-p 8554:8554`, not `-p 9554:8554`). The ports written into URLs are
  the ports the process bound; CamFarm does not know about a remapping. To use other host ports,
  change the ports the fleet binds with `--rtsp-port` and `--onvif-port` and publish those.
- **A client on another machine:** pass `--advertise-host` with a name or IP that machine can
  reach, for example `docker run ... camfarm --cameras 3 --http-addr 0.0.0.0:8080 --listen-host
  0.0.0.0 --rtsp-port 8554 --onvif-port 8000 --advertise-host 192.0.2.10`. Passing arguments
  replaces the image's defaults, so repeat the ones you still want.
- **A client in another container:** use a user-defined network and the service name. The
  repository's `compose.yaml` does this: other services reach `rtsp://camfarm:8554/cam-1`.

`--advertise-host` is part of the spec, so the `spec=sha256:...` in the replay line differs between
the same fleet run in a container and run on the host. Compare replay lines from runs that use the
same listen and advertise settings.

## The dashboard has no authentication

Publish port 8080 on `127.0.0.1` only, as above. The dashboard can start servers, show camera
credentials and report host details, and it is not protected against DNS rebinding. Do not publish
it on a shared network.

## Your own footage

Mount a directory and point the fleet at it:

```sh
docker run --rm -v "$PWD/clips:/clips:ro" \
  -p 127.0.0.1:8080:8080 -p 127.0.0.1:8554:8554 -p 127.0.0.1:8000:8000 \
  camfarm --cameras 3 --http-addr 0.0.0.0:8080 --listen-host 0.0.0.0 \
    --rtsp-port 8554 --onvif-port 8000 --advertise-host 127.0.0.1 --media-dir /clips
```

The image runs as a non-root user, so the mount must be readable by it. Uploads through the
dashboard need a writable media directory, so drop the `:ro` if you want them (see
[the media library](library.md)).

## Discovery

Multicast WS-Discovery commonly does not work across a Docker or containerd (nerdctl) bridge
network, and CamFarm has not measured it on either, so the image does not enable discovery. Your
options:

- **Unicast discovery.** Start the fleet with `--discovery unicast:0.0.0.0:3702`, publish the UDP
  port (`-p 127.0.0.1:3702:3702/udp`) and probe `127.0.0.1:3702` from the host. Without
  `announceAddr` nothing is announced; a client has to probe. CI exercises unicast only.
- **Multicast with host networking** (`--network host` on Linux). It then depends on the host's
  network as described in [WS-Discovery](discovery.md). Docker Desktop on macOS and Windows does
  not provide real host networking.
- **Skip discovery** and give the client the ONVIF endpoint directly, as the README's Scope
  section describes.

Either way, the XAddr in a discovery reply uses `--advertise-host`, so it must be an address the
prober can reach.
