# WS-Discovery

This is a simulation, not a conformance-tested responder. See the Scope section of the
[README](../README.md#scope--what-this-does-not-do) for what it does not support and how multicast
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
