# WS-Discovery

This is a simulation, not a conformance-tested responder. See the Scope section of the
[README](../README.md#scope-what-this-does-not-do) for what it does not support and how multicast
behaves on real networks.

- **Works:** an opt-in WS-Discovery responder, so an ONVIF client can find the fleet by
  probing instead of being handed an endpoint. Set `listen.discovery` in the spec (`mode`
  `multicast` or `unicast`; unicast takes `addr` and optionally `announceAddr`, multicast takes an
  optional `interface`) or pass `--discovery multicast` / `--discovery unicast:HOST:PORT` and
  `--discovery-interface` to `cmd/camfarm`. One socket answers for every camera. Hello (when a
  camera is added or the fleet starts) and Bye (when a camera is removed or the fleet is closed)
  go to the multicast group in multicast mode, or to `announceAddr` in unicast mode; a unicast
  run without `announceAddr`, which is every CLI unicast run, announces nothing. Each camera's endpoint reference is its seed-derived `urn:uuid:` that
  `GetEndpointReference` also returns, and its advertised XAddr is that camera's real ONVIF
  endpoint. `Fleet.DiscoveryAddr()` returns the bound UDP address (the binary prints
  `discovery: udp://ADDR`; in multicast mode it prints the group, `udp://239.255.255.250:3702
  (multicast)`).

## Limits

- **WS-Discovery is a simulation, not a conformance-tested responder.** It is `onvif-go`'s
  responder, which supports Probe/ProbeMatches with Types and Scopes filtering (rfc3986, strcmp0
  and none match rules) plus Hello and Bye. It does not support Resolve/ResolveMatches, the ldap
  and uuid scope rules (such a probe matches nothing), the 2009/01 (WS-Discovery 1.1) namespace,
  or managed mode with a discovery proxy. A wildcard `listen.host` (`0.0.0.0`, `::`) is refused,
  because the host is advertised in each camera's XAddr and a wildcard is not an address a client
  can dial. Multicast mode also refuses a loopback `listen.host`, since it would advertise an
  address no other machine can reach; use unicast mode for loopback. Multicast depends on the
  network: it commonly does not work across Docker bridge networks, on CI runners, between network
  namespaces, or through firewalls and IGMP-snooping switches, and nothing here detects that. CI
  exercises **unicast mode only** for probe and announce behaviour. A test that starts the
  responder in multicast mode (join, add and remove a camera, close) runs only where the host has
  a non-loopback multicast-capable IPv4 interface and skips otherwise, and it does not check that
  any packet is delivered. Multicast joins the
  interface named by `--discovery-interface` (`interface` in the spec); when that is empty
  and `listen.host` is a literal IP it defaults to the interface holding that IP, and with a
  hostname it falls back to the system default-route interface. Containers on a bridge should need the
  bridge's interface (reasoned from how multicast joins work, not tested). **On the same host as the fleet, multicast discovery with `onvif-go`'s
  client (and other clients built on Go's `net.ListenMulticastUDP`) finds nothing and never sees
  Hello or Bye**, because both sides disable `IP_MULTICAST_LOOP`. This was observed by hand on one Linux
  machine (`onvif-go`'s client on the LAN interface found none of the fleet's cameras, while probing
  via `lo` and a Python prober found them); no test covers it. On one host, use the loopback interface (`lo`), a client that leaves multicast loopback
  on, or unicast mode. **Give each fleet on a shared network a distinct seed:** endpoint
  references derive only from the seed (a spec file with no seed, and a zero-value library
  `Spec`, use 0), so two fleets with the same seed advertise identical `urn:uuid:` references
  with different XAddrs, and clients keep one entry per reference. Only the endpoint references
  are seeded: WS-Discovery MessageIDs are random and the AppSequence InstanceId is wall-clock,
  so discovery traffic is not byte-replayable, and reply timing is wall-clock. Enabling
  discovery changes the spec's hash, and so the replay line.
