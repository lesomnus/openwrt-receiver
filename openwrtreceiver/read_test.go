package openwrtreceiver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtures answers the way the router's ubus did; see testdata.
func fixtures(t *testing.T, missing ...string) runner {
	t.Helper()
	files := map[string]string{
		"call system board":                 "system-board.json",
		"call network.interface dump":       "network-interface-dump.json",
		"call network.device status":        "network-device-status.json",
		"call dnsmasq metrics":              "dnsmasq-metrics.json",
		"call hostapd.phy0-ap0 get_clients": "hostapd-phy0-ap0-get_clients.json",
		"call hostapd.phy1-ap0 get_clients": "hostapd-phy1-ap0-get_clients.json",
	}
	return func(_ context.Context, args ...string) ([]byte, error) {
		cmd := strings.Join(args, " ")
		for _, m := range missing {
			if cmd == m {
				return nil, fmt.Errorf("%w: Command failed: ubus %s (Not found)", errNotFound, cmd)
			}
		}
		if cmd == "list hostapd.*" {
			return []byte("hostapd.phy0-ap0\nhostapd.phy1-ap0\n"), nil
		}
		f, ok := files[cmd]
		if !ok {
			return nil, fmt.Errorf("unexpected ubus %s", cmd)
		}
		return os.ReadFile(filepath.Join("testdata", f))
	}
}

// sysfs makes a root whose /sys/class/net/<dev>/carrier_changes holds the given counts.
func sysfs(t *testing.T, counts map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for dev, v := range counts {
		d := filepath.Join(root, "sys", "class", "net", dev)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "carrier_changes"), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func leases(t *testing.T, n int) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "dhcp.leases")
	if err := os.WriteFile(f, []byte(strings.Repeat("1782737960 02:00:00:00:01:00 192.0.2.10 host *\n", n)), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestRead(t *testing.T) {
	r := &reader{run: fixtures(t), sysRoot: sysfs(t, map[string]string{"lan1": "3", "wan": "0"}), leaseFile: leases(t, 2)}
	s, err := r.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if s.board == nil || s.board.Model != "ipTIME AX3000SE" || s.board.Release.Version != "25.12.5" {
		t.Errorf("board: %+v", s.board)
	}
	if len(s.interfaces) != 5 {
		t.Fatalf("interfaces: %+v", s.interfaces)
	}
	var wwan iface
	for _, i := range s.interfaces {
		if i.Name == "wwan" {
			wwan = i
		}
	}
	if !wwan.Up || wwan.Uptime == nil || *wwan.Uptime != 36196 || wwan.Proto != "dhcp" || wwan.L3Device != "phy1-sta0" {
		t.Errorf("wwan: %+v", wwan)
	}
	if d := s.devices["lan1"]; !d.Present || !d.Carrier || d.Speed != "1000F" || d.DevType != "dsa" {
		t.Errorf("lan1: %+v", d)
	}
	if got := s.carrierChanges; len(got) != 2 || got["lan1"] != 3 || got["wan"] != 0 {
		t.Errorf("carrier changes: %v", got)
	}
	if s.dnsmasq["dns_queries_forwarded"] != 1710 || s.leases != 2 {
		t.Errorf("dnsmasq %v, leases %d", s.dnsmasq, s.leases)
	}
	if len(s.clients) != 1 {
		t.Fatalf("clients: %+v", s.clients)
	}
	c := s.clients[0]
	if c.Interface != "phy1-ap0" || c.MAC != "02:00:00:00:00:07" || !c.Authorized || !c.VHT || c.HE || c.streams() != 2 || c.rrm() {
		t.Errorf("client: %+v, streams %d", c, c.streams())
	}
}

func TestReadPartial(t *testing.T) {
	// dnsmasq not running is not a failure; a broken reply is.
	r := &reader{run: fixtures(t, "call dnsmasq metrics"), sysRoot: t.TempDir(), leaseFile: filepath.Join(t.TempDir(), "none")}
	s, err := r.read(context.Background())
	if err != nil {
		t.Fatalf("not found reads as absent: %v", err)
	}
	if s.dnsmasq != nil || s.leases != -1 || s.board == nil {
		t.Errorf("dnsmasq %v, leases %d", s.dnsmasq, s.leases)
	}

	broken := func(_ context.Context, args ...string) ([]byte, error) {
		if strings.Join(args, " ") == "call network.device status" {
			return []byte("{"), nil
		}
		return fixtures(t)(context.Background(), args...)
	}
	r = &reader{run: broken, sysRoot: t.TempDir(), leaseFile: leases(t, 0)}
	s, err = r.read(context.Background())
	if err == nil || !strings.Contains(err.Error(), "network.device status") {
		t.Errorf("error: %v", err)
	}
	if s.board == nil || len(s.interfaces) != 5 || len(s.devices) != 0 {
		t.Errorf("the rest is read: %+v", s)
	}
}

func TestSpeed(t *testing.T) {
	for in, want := range map[string]struct {
		bps    float64
		duplex string
		ok     bool
	}{
		"1000F": {1e9, "full", true},
		"2500F": {2.5e9, "full", true},
		"100H":  {1e8, "half", true},
		"":      {0, "", false},
		"1000":  {0, "", false},
		"-1F":   {0, "", false},
	} {
		bps, duplex, ok := speed(in)
		if bps != want.bps || duplex != want.duplex || ok != want.ok {
			t.Errorf("%q: %v %q %v", in, bps, duplex, ok)
		}
	}
}

func TestCollect(t *testing.T) {
	r := &reader{run: fixtures(t), sysRoot: sysfs(t, map[string]string{"lan1": "3"}), leaseFile: leases(t, 2)}
	s, err := r.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s.devices["lan2"].Statistics["rx_crc_errors"] = 4
	points := collect(s)

	find := func(d *desc, want attrs) (float64, bool) {
		for _, p := range points {
			if p.d != d || len(p.attrs) != len(want) {
				continue
			}
			match := true
			for k, v := range want {
				if p.attrs[k] != v {
					match = false
				}
			}
			if match {
				return p.value, true
			}
		}
		return 0, false
	}
	dev := func(name, typ string, kvs ...any) attrs {
		a := attrs{"network.interface.name": name}
		if typ != "" {
			a["openwrt.device.type"] = typ
		}
		return with(a, kvs...)
	}
	for _, w := range []struct {
		d     *desc
		attrs attrs
		value float64
	}{
		{interfaceUp, attrs{"openwrt.interface.name": "wwan", "openwrt.interface.proto": "dhcp"}, 1},
		{interfaceUptime, attrs{"openwrt.interface.name": "wwan", "openwrt.interface.proto": "dhcp"}, 36196},
		{interfaceUp, attrs{"openwrt.interface.name": "wan", "openwrt.interface.proto": "dhcp"}, 0},
		{deviceCarrier, dev("lan1", "dsa"), 1},
		{deviceCarrier, dev("lan4", "dsa"), 0},
		{deviceSpeed, dev("lan1", "dsa", "openwrt.device.duplex", "full"), 1e9},
		{deviceSpeed, dev("eth0", "ethernet", "openwrt.device.duplex", "full"), 2.5e9},
		{deviceCarrierChanges, dev("lan1", "dsa"), 3},
		{deviceErrors, dev("lan2", "dsa", "network.io.direction", "receive", "error.type", "crc"), 4},
		{dnsQueries, attrs{"openwrt.dns.answer": "forwarded"}, 1710},
		{dnsCacheInsertions, nil, 2614},
		{dhcpMessages, attrs{"openwrt.dhcp.message.type": "ack"}, 2},
		{dhcpLeaseCount, nil, 2},
		{dhcpLeaseAllocations, attrs{"network.type": "ipv4"}, 1},
		{wifiClientStreams, attrs{"network.interface.name": "phy1-ap0", "wifi.station.mac": "02:00:00:00:00:07"}, 2},
	} {
		v, ok := find(w.d, w.attrs)
		if !ok || v != w.value {
			t.Errorf("%s %v: got %v (found %v), want %v", w.d.Name, w.attrs, v, ok, w.value)
		}
	}
	for _, w := range []struct {
		d     *desc
		attrs attrs
	}{
		{interfaceUptime, attrs{"openwrt.interface.name": "wan", "openwrt.interface.proto": "dhcp"}}, // down
		{deviceSpeed, dev("lan4", "dsa", "openwrt.device.duplex", "full")},                           // no link
		{deviceErrors, dev("lan1", "dsa", "network.io.direction", "receive", "error.type", "crc")},   // none
	} {
		if v, ok := find(w.d, w.attrs); ok {
			t.Errorf("%s %v: want none, got %v", w.d.Name, w.attrs, v)
		}
	}
	info := 0
	for _, p := range points {
		if p.d == wifiClientInfo {
			info++
			if p.attrs["openwrt.wifi.client.vht"] != true || p.attrs["openwrt.wifi.client.he"] != false {
				t.Errorf("client info: %v", p.attrs)
			}
		}
	}
	if info != 1 {
		t.Errorf("want one client, got %d", info)
	}
}
