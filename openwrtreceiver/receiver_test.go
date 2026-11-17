package openwrtreceiver_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lesomnus/openwrt-receiver/openwrtreceiver"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver/receivertest"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// ubus answers from testdata, the way the router's did.
func ubus(_ context.Context, args ...string) ([]byte, error) {
	cmd := strings.Join(args, " ")
	if cmd == "list hostapd.*" {
		return []byte("hostapd.phy0-ap0\nhostapd.phy1-ap0\n"), nil
	}
	name := strings.NewReplacer("call ", "", ".", "-", " ", "-").Replace(cmd)
	b, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", openwrtreceiver.ErrNotFound, cmd)
	}
	return b, nil
}

func TestConfig(t *testing.T) {
	f := openwrtreceiver.NewFactory()
	if err := componenttest.CheckConfigStruct(f.CreateDefaultConfig()); err != nil {
		t.Fatal(err)
	}
	c := f.CreateDefaultConfig().(*openwrtreceiver.Config)
	if err := c.Validate(); err != nil {
		t.Errorf("default: %v", err)
	}
	c.RootPath = "hostfs"
	if c.Validate() == nil {
		t.Error("a relative root path is valid")
	}
}

func start(t *testing.T, find func(string) (openwrtreceiver.Runner, error)) (*consumertest.MetricsSink, *observer.ObservedLogs, func()) {
	t.Helper()
	t.Cleanup(openwrtreceiver.SetRunner(find))
	f := openwrtreceiver.NewFactory()
	c := f.CreateDefaultConfig().(*openwrtreceiver.Config)
	c.CollectionInterval = 20 * time.Millisecond
	c.InitialDelay = 0
	c.RootPath = t.TempDir()
	c.LeaseFile = filepath.Join(t.TempDir(), "dhcp.leases")
	if err := os.WriteFile(c.LeaseFile, []byte("1 02:00:00:00:01:00 192.0.2.10 host *\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.InfoLevel)
	set := receivertest.NewNopSettings(openwrtreceiver.Type)
	set.Logger = zap.New(core)
	sink := new(consumertest.MetricsSink)
	r, err := f.CreateMetrics(t.Context(), set, c, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(t.Context(), componenttest.NewNopHost()); err != nil {
		t.Fatal(err)
	}
	return sink, logs, func() {
		if err := r.Shutdown(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReceiver(t *testing.T) {
	sink, logs, stop := start(t, func(string) (openwrtreceiver.Runner, error) { return ubus, nil })
	deadline := time.Now().Add(5 * time.Second)
	for len(sink.AllMetrics()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("want two scrapes")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	if logs.Len() != 0 {
		t.Errorf("want no logs: %v", logs.All())
	}

	first, second := names(sink.AllMetrics()[0]), names(sink.AllMetrics()[1])
	for name, want := range map[string]pmetric.MetricType{
		"openwrt.system.info":                 pmetric.MetricTypeGauge,
		"openwrt.interface.up":                pmetric.MetricTypeGauge,
		"openwrt.interface.uptime":            pmetric.MetricTypeGauge,
		"openwrt.device.carrier":              pmetric.MetricTypeGauge,
		"openwrt.device.speed":                pmetric.MetricTypeGauge,
		"openwrt.dns.queries":                 pmetric.MetricTypeSum,
		"openwrt.dns.cache.insertions":        pmetric.MetricTypeSum,
		"openwrt.dhcp.messages":               pmetric.MetricTypeSum,
		"openwrt.dhcp.lease.count":            pmetric.MetricTypeGauge,
		"openwrt.dhcp.lease.allocations":      pmetric.MetricTypeSum,
		"openwrt.wifi.client.info":            pmetric.MetricTypeGauge,
		"openwrt.wifi.client.spatial_streams": pmetric.MetricTypeGauge,
	} {
		m, ok := first[name]
		if !ok {
			t.Errorf("%s: missing", name)
			continue
		}
		if m.Type() != want {
			t.Errorf("%s: %s, want %s", name, m.Type(), want)
		}
		if want == pmetric.MetricTypeSum {
			if !m.Sum().IsMonotonic() || m.Sum().AggregationTemporality() != pmetric.AggregationTemporalityCumulative {
				t.Errorf("%s: not a monotonic cumulative sum", name)
			}
			a := m.Sum().DataPoints().At(0)
			b := second[name].Sum().DataPoints().At(0)
			if a.StartTimestamp() == 0 || a.StartTimestamp() > a.Timestamp() || a.StartTimestamp() != b.StartTimestamp() {
				t.Errorf("%s: start %v then %v", name, a.StartTimestamp(), b.StartTimestamp())
			}
		}
	}
	dp := first["openwrt.wifi.client.info"].Gauge().DataPoints().At(0)
	if v, ok := dp.Attributes().Get("openwrt.wifi.client.vht"); !ok || !v.Bool() {
		t.Errorf("client attributes keep their types: %v", dp.Attributes().AsRaw())
	}
}

func TestNotAvailable(t *testing.T) {
	var mu sync.Mutex
	tries := 0
	sink, logs, stop := start(t, func(string) (openwrtreceiver.Runner, error) {
		mu.Lock()
		defer mu.Unlock()
		tries++
		return nil, fmt.Errorf("%w: no ubus", openwrtreceiver.ErrNotAvailable)
	})
	time.Sleep(150 * time.Millisecond)
	stop()
	if n := logs.FilterMessageSnippet("not available").Len(); n != 1 {
		t.Errorf("want one idle message, got %d: %v", n, logs.All())
	}
	if n := sink.DataPointCount(); n != 0 {
		t.Errorf("want no data points, got %d", n)
	}
	if mu.Lock(); tries < 2 {
		t.Errorf("want it tried again each scrape, tried %d times", tries)
	}
	mu.Unlock()
}

func names(md pmetric.Metrics) map[string]pmetric.Metric {
	m := map[string]pmetric.Metric{}
	ms := md.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics()
	for i := range ms.Len() {
		m[ms.At(i).Name()] = ms.At(i)
	}
	return m
}
