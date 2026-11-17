# openwrt-receiver

An [OpenTelemetry Collector](https://github.com/open-telemetry/opentelemetry-collector) receiver that reads the state of an [OpenWrt](https://openwrt.org) router from ubus:
its logical interfaces, its network devices and their links, dnsmasq's DNS and DHCP, the clients of its access points, and the release it runs.
It runs on the router, inside a Collector built for it.

For the WiFi links themselves (signal, rates, airtime, channel use), see [nl80211-exporter](https://github.com/lesomnus/nl80211-exporter).

| Status    |         |
| --------- | ------- |
| Stability | alpha   |
| Signals   | metrics |

## Build

Add it to an [ocb](https://github.com/open-telemetry/opentelemetry-collector/tree/main/cmd/builder) manifest:

```yaml
receivers:
  - gomod: github.com/lesomnus/openwrt-receiver/openwrtreceiver v0.0.0-<date>-<commit>
```

Use a pseudo-version of a commit on `main`, e.g. from `go list -m github.com/lesomnus/openwrt-receiver/openwrtreceiver@main`.
It needs Collector v0.163.0 or later.

## Configuration

```yaml
receivers:
  openwrt:
    collection_interval: 15s
```

| Field                 | Default            | Description                                                         |
| --------------------- | ------------------ | ------------------------------------------------------------------- |
| `collection_interval` | `1m`               | How often to read.                                                  |
| `initial_delay`       | `1s`               | Delay before the first read.                                        |
| `timeout`             | `0s`               | Timeout of each read; `0s` means no timeout.                        |
| `ubus`                | `ubus`             | The ubus tool, which every call goes through.                       |
| `root_path`           |                    | Read `<root_path>/sys` instead of `/sys`, like `hostmetrics`.        |
| `lease_file`          | `/tmp/dhcp.leases` | dnsmasq's lease file: one line per DHCP lease.                      |

Where there is no ubus, the receiver logs that once and reports nothing, so the same configuration can run on nodes that are not OpenWrt.
A part that is not there, like dnsmasq on a router that runs none, is left out quietly; any other part that fails is reported as a partial scrape error, with the rest still sent.

## Metrics

| Metric                                | Type    | Unit           | Attributes | Description |
| ------------------------------------- | ------- | -------------- | ---------- | ----------- |
| `openwrt.system.info`                 | gauge   |                | `openwrt.model`, `openwrt.board`, `openwrt.version`, `openwrt.revision`, `openwrt.target`, `openwrt.kernel` | The OpenWrt release and the board it runs on. Always 1. |
| `openwrt.interface.up`                | gauge   |                | interface  | Whether a logical interface of netifd is up: 1 or 0. |
| `openwrt.interface.uptime`            | gauge   | `s`            | interface  | Time since a logical interface came up. Only while it is up. |
| `openwrt.device.carrier`              | gauge   |                | device     | Whether a network device has a link: 1 or 0. |
| `openwrt.device.speed`                | gauge   | `bit/s`        | device, `openwrt.device.duplex` | Link speed a network device negotiated. Only while it has a link. |
| `openwrt.device.carrier_changes`      | counter | `{change}`     | device     | Times the link of a network device went up or down. |
| `openwrt.device.errors`               | counter | `{error}`      | device, `network.io.direction`, `error.type` | Errors of a network device, by type. Only types that occurred are reported. |
| `openwrt.dns.queries`                 | counter | `{query}`      | `openwrt.dns.answer` | DNS queries dnsmasq handled, by how it answered: `forwarded`, `local`, `authoritative`, `stale`, or `unanswered`. |
| `openwrt.dns.cache.insertions`        | counter | `{entry}`      |            | Entries dnsmasq put in its DNS cache. |
| `openwrt.dns.cache.evictions`         | counter | `{entry}`      |            | Entries dnsmasq dropped from its full cache before they expired. |
| `openwrt.dns.tcp_connections`         | gauge   | `{connection}` |            | Open TCP connections to dnsmasq's DNS server. |
| `openwrt.dhcp.messages`               | counter | `{message}`    | `openwrt.dhcp.message.type` | DHCP and BOOTP messages, by type: `discover`, `offer`, `request`, `ack`, `nak`, `decline`, `release`, `inform`, `bootp`, `pxe`. |
| `openwrt.dhcp.unanswered`             | counter | `{message}`    |            | DHCP messages dnsmasq did not answer. |
| `openwrt.dhcp.lease.count`            | gauge   | `{lease}`      |            | DHCP leases in the lease file. |
| `openwrt.dhcp.lease.allocations`      | counter | `{lease}`      | `network.type` | DHCP leases dnsmasq allocated, `ipv4` or `ipv6`. |
| `openwrt.dhcp.lease.prunes`           | counter | `{lease}`      | `network.type` | DHCP leases dnsmasq removed once they expired. |
| `openwrt.wifi.client.info`            | gauge   |                | client, `openwrt.wifi.client.{authorized,wmm,ht,vht,he,mfp,wps,mbo,rrm}` | What a client of an access point negotiated and supports, as booleans. Always 1. |
| `openwrt.wifi.client.spatial_streams` | gauge   | `{stream}`     | client     | Spatial streams a client can receive, from its HE or VHT capabilities. |

- **interface**: `openwrt.interface.name` (e.g. `lan`, `wan`, `wwan`) and `openwrt.interface.proto` (e.g. `static`, `dhcp`).
  A logical interface that keeps coming back up shows as `openwrt.interface.uptime` falling to zero.
- **device**: `network.interface.name` (e.g. `lan1`, `wan`, `br-lan`, `phy1-ap0`) and `openwrt.device.type` (e.g. `dsa` for a switch port, `ethernet`, `bridge`) when netifd knows it.
  `error.type` is `crc`, `frame`, `length`, `missed`, `overrun`, or `fifo` received, and `aborted`, `carrier`, `fifo`, `heartbeat`, `window`, or `collision` transmitted; CRC errors on a port usually mean its cable.
- **client**: `network.interface.name`, the access point's interface, and `wifi.station.mac`, the client's MAC address as `iw` and `hostapd` print it, the same as nl80211-exporter's station metrics.

dnsmasq counts from its start, so its counters reset when it restarts; a counter that goes down starts again at the previous scrape.
Counters start when the receiver does, or, for a series that appears or resets later, at the previous scrape.

## How it reads

Every read runs the ubus tool:

| Call                                   | Used for |
| -------------------------------------- | -------- |
| `ubus call system board`               | `openwrt.system.info` |
| `ubus call network.interface dump`     | `openwrt.interface.*` |
| `ubus call network.device status`      | `openwrt.device.*`, besides the link changes |
| `ubus call dnsmasq metrics`            | `openwrt.dns.*`, `openwrt.dhcp.*` |
| `ubus list hostapd.*`, then `ubus call hostapd.<interface> get_clients` for each | `openwrt.wifi.client.*` |

It also reads `/sys/class/net/<device>/carrier_changes` for each device netifd lists, and counts the lines of the lease file.
All of these only read. Tried on an ipTIME AX3000SE (MediaTek MT7981) running OpenWrt 25.12; the tests replay what its ubus answered, with the MAC and IP addresses replaced.
