package openwrtreceiver

import (
	"fmt"
	"sort"
	"strings"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

type desc struct {
	Name        string
	Description string
	Unit        string
	Counter     bool // monotonic and cumulative; integers either way
}

var (
	systemInfo = &desc{Name: "openwrt.system.info",
		Description: "The OpenWrt release and the board it runs on, as attributes. Always 1."}
	interfaceUp = &desc{Name: "openwrt.interface.up",
		Description: "Whether a logical interface of netifd is up: 1 or 0."}
	interfaceUptime = &desc{Name: "openwrt.interface.uptime", Unit: "s",
		Description: "Time since a logical interface of netifd came up."}
	deviceCarrier = &desc{Name: "openwrt.device.carrier",
		Description: "Whether a network device has a link: 1 or 0."}
	deviceSpeed = &desc{Name: "openwrt.device.speed", Unit: "bit/s",
		Description: "Link speed a network device negotiated."}
	deviceCarrierChanges = &desc{Name: "openwrt.device.carrier_changes", Unit: "{change}", Counter: true,
		Description: "Times the link of a network device went up or down."}
	deviceErrors = &desc{Name: "openwrt.device.errors", Unit: "{error}", Counter: true,
		Description: "Errors of a network device, by type. Only types that occurred are reported."}
	dnsQueries = &desc{Name: "openwrt.dns.queries", Unit: "{query}", Counter: true,
		Description: "DNS queries dnsmasq handled, by how it answered."}
	dnsCacheInsertions = &desc{Name: "openwrt.dns.cache.insertions", Unit: "{entry}", Counter: true,
		Description: "Entries dnsmasq put in its DNS cache."}
	dnsCacheEvictions = &desc{Name: "openwrt.dns.cache.evictions", Unit: "{entry}", Counter: true,
		Description: "Entries dnsmasq dropped from its full DNS cache before they expired."}
	dnsTCPConnections = &desc{Name: "openwrt.dns.tcp_connections", Unit: "{connection}",
		Description: "Open TCP connections to dnsmasq's DNS server."}
	dhcpMessages = &desc{Name: "openwrt.dhcp.messages", Unit: "{message}", Counter: true,
		Description: "DHCP and BOOTP messages dnsmasq received or sent, by type."}
	dhcpUnanswered = &desc{Name: "openwrt.dhcp.unanswered", Unit: "{message}", Counter: true,
		Description: "DHCP messages dnsmasq did not answer."}
	dhcpLeaseCount = &desc{Name: "openwrt.dhcp.lease.count", Unit: "{lease}",
		Description: "DHCP leases in dnsmasq's lease file."}
	dhcpLeaseAllocations = &desc{Name: "openwrt.dhcp.lease.allocations", Unit: "{lease}", Counter: true,
		Description: "DHCP leases dnsmasq allocated."}
	dhcpLeasePrunes = &desc{Name: "openwrt.dhcp.lease.prunes", Unit: "{lease}", Counter: true,
		Description: "DHCP leases dnsmasq removed once they expired."}
	wifiClientInfo = &desc{Name: "openwrt.wifi.client.info",
		Description: "What a client of an access point negotiated and supports, as attributes. Always 1."}
	wifiClientStreams = &desc{Name: "openwrt.wifi.client.spatial_streams", Unit: "{stream}",
		Description: "Spatial streams a client of an access point can receive, from its capabilities."}
)

// all lists every metric, in the order they are reported.
var all = []*desc{
	systemInfo, interfaceUp, interfaceUptime,
	deviceCarrier, deviceSpeed, deviceCarrierChanges, deviceErrors,
	dnsQueries, dnsCacheInsertions, dnsCacheEvictions, dnsTCPConnections,
	dhcpMessages, dhcpUnanswered, dhcpLeaseCount, dhcpLeaseAllocations, dhcpLeasePrunes,
	wifiClientInfo, wifiClientStreams,
}

// dnsmasq's metric names, by what they become.
var (
	dnsAnswers = map[string]string{
		"dns_queries_forwarded": "forwarded",
		"dns_local_answered":    "local",
		"dns_auth_answered":     "authoritative",
		"dns_stale_answered":    "stale",
		"dns_unanswered":        "unanswered",
	}
	dhcpTypes = map[string]string{
		"dhcp_discover": "discover",
		"dhcp_offer":    "offer",
		"dhcp_request":  "request",
		"dhcp_ack":      "ack",
		"dhcp_nak":      "nak",
		"dhcp_decline":  "decline",
		"dhcp_release":  "release",
		"dhcp_inform":   "inform",
		"bootp":         "bootp",
		"pxe":           "pxe",
	}
	leaseFamilies = map[string]string{"4": "ipv4", "6": "ipv6"}
	// Statistics of a network device that count errors, by direction and type.
	deviceErrorTypes = map[string][2]string{
		"rx_crc_errors":       {"receive", "crc"},
		"rx_frame_errors":     {"receive", "frame"},
		"rx_length_errors":    {"receive", "length"},
		"rx_missed_errors":    {"receive", "missed"},
		"rx_over_errors":      {"receive", "overrun"},
		"rx_fifo_errors":      {"receive", "fifo"},
		"tx_aborted_errors":   {"transmit", "aborted"},
		"tx_carrier_errors":   {"transmit", "carrier"},
		"tx_fifo_errors":      {"transmit", "fifo"},
		"tx_heartbeat_errors": {"transmit", "heartbeat"},
		"tx_window_errors":    {"transmit", "window"},
		"collisions":          {"transmit", "collision"},
	}
)

// attrs are a point's attributes; values are string, int64, or bool.
type attrs map[string]any

type point struct {
	d     *desc
	attrs attrs
	value float64
}

func collect(s *snapshot) []point {
	var ps []point
	add := func(d *desc, v float64, a attrs) {
		ps = append(ps, point{d: d, attrs: a, value: v})
	}
	b01 := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}

	if b := s.board; b != nil {
		add(systemInfo, 1, attrs{
			"openwrt.model": b.Model, "openwrt.board": b.BoardName, "openwrt.kernel": b.Kernel,
			"openwrt.version": b.Release.Version, "openwrt.revision": b.Release.Revision, "openwrt.target": b.Release.Target,
		})
	}

	for _, i := range s.interfaces {
		a := attrs{"openwrt.interface.name": i.Name, "openwrt.interface.proto": i.Proto}
		add(interfaceUp, b01(i.Up), a)
		if i.Up && i.Uptime != nil {
			add(interfaceUptime, float64(*i.Uptime), a)
		}
	}

	for _, name := range sortedKeys(s.devices) {
		d := s.devices[name]
		if !d.Present {
			continue
		}
		a := attrs{"network.interface.name": name}
		if d.DevType != "" {
			a["openwrt.device.type"] = d.DevType
		}
		add(deviceCarrier, b01(d.Carrier), a)
		if v, duplex, ok := speed(d.Speed); ok && d.Carrier {
			add(deviceSpeed, v, with(a, "openwrt.device.duplex", duplex))
		}
		if v, ok := s.carrierChanges[name]; ok {
			add(deviceCarrierChanges, float64(v), a)
		}
		for _, stat := range sortedKeys(deviceErrorTypes) {
			if v := d.Statistics[stat]; v > 0 {
				t := deviceErrorTypes[stat]
				add(deviceErrors, float64(v), with(with(a, "network.io.direction", t[0]), "error.type", t[1]))
			}
		}
	}

	if m := s.dnsmasq; m != nil {
		for _, k := range sortedKeys(dnsAnswers) {
			if v, ok := m[k]; ok {
				add(dnsQueries, float64(v), attrs{"openwrt.dns.answer": dnsAnswers[k]})
			}
		}
		if v, ok := m["dns_cache_inserted"]; ok {
			add(dnsCacheInsertions, float64(v), nil)
		}
		if v, ok := m["dns_cache_live_freed"]; ok {
			add(dnsCacheEvictions, float64(v), nil)
		}
		if v, ok := m["tcp_connections"]; ok {
			add(dnsTCPConnections, float64(v), nil)
		}
		for _, k := range sortedKeys(dhcpTypes) {
			if v, ok := m[k]; ok {
				add(dhcpMessages, float64(v), attrs{"openwrt.dhcp.message.type": dhcpTypes[k]})
			}
		}
		if v, ok := m["noanswer"]; ok {
			add(dhcpUnanswered, float64(v), nil)
		}
		for _, f := range sortedKeys(leaseFamilies) {
			a := attrs{"network.type": leaseFamilies[f]}
			if v, ok := m["leases_allocated_"+f]; ok {
				add(dhcpLeaseAllocations, float64(v), a)
			}
			if v, ok := m["leases_pruned_"+f]; ok {
				add(dhcpLeasePrunes, float64(v), a)
			}
		}
	}
	if s.leases >= 0 {
		add(dhcpLeaseCount, float64(s.leases), nil)
	}

	for _, c := range s.clients {
		a := attrs{"network.interface.name": c.Interface, "wifi.station.mac": c.MAC}
		add(wifiClientInfo, 1, with(a, "openwrt.wifi.client.authorized", c.Authorized,
			"openwrt.wifi.client.wmm", c.WMM, "openwrt.wifi.client.ht", c.HT, "openwrt.wifi.client.vht", c.VHT,
			"openwrt.wifi.client.he", c.HE, "openwrt.wifi.client.mfp", c.MFP, "openwrt.wifi.client.wps", c.WPS,
			"openwrt.wifi.client.mbo", c.MBO, "openwrt.wifi.client.rrm", c.rrm()))
		if n := c.streams(); n > 0 {
			add(wifiClientStreams, float64(n), a)
		}
	}

	// In the order of all.
	order := map[*desc]int{}
	for i, d := range all {
		order[d] = i
	}
	sort.SliceStable(ps, func(i, j int) bool { return order[ps[i].d] < order[ps[j].d] })
	return ps
}

// with copies a and sets the given key-value pairs.
func with(a attrs, kvs ...any) attrs {
	out := make(attrs, len(a)+len(kvs)/2)
	for k, v := range a {
		out[k] = v
	}
	for i := 0; i+1 < len(kvs); i += 2 {
		out[kvs[i].(string)] = kvs[i+1]
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// key identifies a series: its metric and attributes.
func (p point) key() string {
	ks := sortedKeys(p.attrs)
	var b strings.Builder
	b.WriteString(p.d.Name)
	for _, k := range ks {
		fmt.Fprintf(&b, "\x00%s=%v", k, p.attrs[k])
	}
	return b.String()
}

// starts keeps when each counter series started: when the receiver did, or,
// for a series that appeared or reset later, at the previous scrape.
type starts struct {
	first pcommon.Timestamp
	prev  pcommon.Timestamp
	seen  map[string]series
}

type series struct {
	start pcommon.Timestamp
	last  float64
}

func newStarts(first pcommon.Timestamp) *starts {
	return &starts{first: first, prev: first, seen: map[string]series{}}
}

func toMetrics(points []point, st *starts, ts pcommon.Timestamp) pmetric.Metrics {
	md := pmetric.NewMetrics()
	sm := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty()
	sm.Scope().SetName(ScopeName)

	seen := map[string]series{}
	var (
		cur *desc
		dps pmetric.NumberDataPointSlice
	)
	for _, p := range points {
		if p.d != cur {
			cur = p.d
			m := sm.Metrics().AppendEmpty()
			m.SetName(cur.Name)
			m.SetDescription(cur.Description)
			m.SetUnit(cur.Unit)
			if cur.Counter {
				sum := m.SetEmptySum()
				sum.SetIsMonotonic(true)
				sum.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
				dps = sum.DataPoints()
			} else {
				dps = m.SetEmptyGauge().DataPoints()
			}
		}
		dp := dps.AppendEmpty()
		dp.SetTimestamp(ts)
		dp.SetIntValue(int64(p.value))
		for k, v := range p.attrs {
			switch v := v.(type) {
			case string:
				dp.Attributes().PutStr(k, v)
			case bool:
				dp.Attributes().PutBool(k, v)
			case int64:
				dp.Attributes().PutInt(k, v)
			default:
				dp.Attributes().PutStr(k, fmt.Sprint(v))
			}
		}
		if cur.Counter && st != nil {
			k := p.key()
			s, ok := st.seen[k]
			switch {
			case !ok && st.prev == st.first:
				s.start = st.first
			case !ok || p.value < s.last:
				// New since the previous scrape, or reset, e.g. dnsmasq restarted.
				s.start = st.prev
			}
			s.last = p.value
			seen[k] = s
			dp.SetStartTimestamp(s.start)
		}
	}
	if st != nil {
		st.seen = seen
		st.prev = ts
	}
	return md
}
