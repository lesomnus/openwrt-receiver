package openwrtreceiver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// errNotAvailable means there is no ubus to ask, i.e. this is not OpenWrt.
var errNotAvailable = errors.New("ubus is not available")

// runner runs the ubus tool with args and returns what it prints; tests stand in for it.
type runner func(ctx context.Context, args ...string) ([]byte, error)

// errNotFound is what ubus says of an object or method that is not there, e.g. dnsmasq not running.
var errNotFound = errors.New("not found")

func execRunner(path string) (runner, error) {
	p, err := exec.LookPath(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errNotAvailable, err)
	}
	return func(ctx context.Context, args ...string) ([]byte, error) {
		var stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, p, args...)
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			msg := strings.TrimSpace(stderr.String())
			if strings.Contains(msg, "Not found") {
				return nil, fmt.Errorf("%w: %s", errNotFound, msg)
			}
			if msg != "" {
				return nil, fmt.Errorf("%w: %s", err, msg)
			}
			return nil, err
		}
		return out, nil
	}, nil
}

// snapshot is what one read found. A part that could not be read is nil or empty.
type snapshot struct {
	time       time.Time
	board      *board
	interfaces []iface
	devices    map[string]device
	// Link changes of each device, from sysfs.
	carrierChanges map[string]uint64
	dnsmasq        map[string]uint64
	// Lines in dnsmasq's lease file; -1 when there is none.
	leases  int
	clients []client
}

type board struct {
	Kernel    string `json:"kernel"`
	Model     string `json:"model"`
	BoardName string `json:"board_name"`
	Release   struct {
		Version  string `json:"version"`
		Revision string `json:"revision"`
		Target   string `json:"target"`
	} `json:"release"`
}

// iface is a logical interface of netifd, e.g. lan, wan, or wwan.
type iface struct {
	Name     string `json:"interface"`
	Up       bool   `json:"up"`
	Uptime   *int64 `json:"uptime"`
	Proto    string `json:"proto"`
	L3Device string `json:"l3_device"`
}

// device is a network device netifd knows, e.g. a switch port, a bridge, or a WiFi interface.
type device struct {
	Present    bool              `json:"present"`
	Carrier    bool              `json:"carrier"`
	Speed      string            `json:"speed"` // e.g. 1000F
	DevType    string            `json:"devtype"`
	Statistics map[string]uint64 `json:"statistics"`
}

// client is a station of one of hostapd's access points.
type client struct {
	Interface string
	MAC       string
	clientInfo
}

type clientInfo struct {
	Authorized   bool  `json:"authorized"`
	WMM          bool  `json:"wmm"`
	HT           bool  `json:"ht"`
	VHT          bool  `json:"vht"`
	HE           bool  `json:"he"`
	MFP          bool  `json:"mfp"`
	WPS          bool  `json:"wps"`
	MBO          bool  `json:"mbo"`
	RRM          []int `json:"rrm"`
	Capabilities struct {
		VHT *mcsMaps `json:"vht"`
		HE  *mcsMaps `json:"he"`
	} `json:"capabilities"`
}

type mcsMaps struct {
	MCSMap struct {
		RX map[string]int `json:"rx"` // per number of spatial streams, e.g. 2ss; -1 when not supported
	} `json:"mcs_map"`
}

// streams is how many spatial streams the client can receive, from its highest capabilities; 0 when unknown.
func (c clientInfo) streams() int {
	for _, m := range []*mcsMaps{c.Capabilities.HE, c.Capabilities.VHT} {
		if m == nil {
			continue
		}
		n := 0
		for k, v := range m.MCSMap.RX {
			if i, err := strconv.Atoi(strings.TrimSuffix(k, "ss")); err == nil && v >= 0 && i > n {
				n = i
			}
		}
		if n > 0 {
			return n
		}
	}
	return 0
}

func (c clientInfo) rrm() bool {
	for _, b := range c.RRM {
		if b != 0 {
			return true
		}
	}
	return false
}

type reader struct {
	run       runner
	sysRoot   string
	leaseFile string
}

func (r *reader) call(ctx context.Context, v any, object, method string) error {
	out, err := r.run(ctx, "call", object, method)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("%s %s: %w", object, method, err)
	}
	return nil
}

// read asks ubus for each part. A part that is not there, e.g. dnsmasq on a node
// that runs none, is left out; any other failure leaves the rest and says so.
func (r *reader) read(ctx context.Context) (*snapshot, error) {
	s := &snapshot{time: time.Now(), leases: -1}
	var errs []error
	note := func(what string, err error) {
		if err != nil && !errors.Is(err, errNotFound) {
			errs = append(errs, fmt.Errorf("%s: %w", what, err))
		}
	}

	var b board
	if err := r.call(ctx, &b, "system", "board"); err == nil {
		s.board = &b
	} else {
		note("system board", err)
	}

	var dump struct {
		Interface []iface `json:"interface"`
	}
	if err := r.call(ctx, &dump, "network.interface", "dump"); err == nil {
		s.interfaces = dump.Interface
	} else {
		note("network.interface dump", err)
	}

	if err := r.call(ctx, &s.devices, "network.device", "status"); err != nil {
		note("network.device status", err)
	}
	s.carrierChanges = map[string]uint64{}
	for name, d := range s.devices {
		if !d.Present {
			continue
		}
		b, err := os.ReadFile(filepath.Join(r.sysRoot, "sys", "class", "net", name, "carrier_changes"))
		if err != nil {
			continue // e.g. gone since netifd listed it
		}
		if v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err == nil {
			s.carrierChanges[name] = v
		}
	}

	if err := r.call(ctx, &s.dnsmasq, "dnsmasq", "metrics"); err != nil {
		note("dnsmasq metrics", err)
	}
	if b, err := os.ReadFile(r.leaseFile); err == nil {
		s.leases = bytes.Count(b, []byte{'\n'})
	}

	out, err := r.run(ctx, "list", "hostapd.*")
	if err != nil {
		note("hostapd list", err)
	}
	for _, object := range strings.Fields(string(out)) {
		var c struct {
			Clients map[string]clientInfo `json:"clients"`
		}
		if err := r.call(ctx, &c, object, "get_clients"); err != nil {
			note(object+" get_clients", err)
			continue
		}
		macs := make([]string, 0, len(c.Clients))
		for mac := range c.Clients {
			macs = append(macs, mac)
		}
		sort.Strings(macs)
		for _, mac := range macs {
			s.clients = append(s.clients, client{Interface: strings.TrimPrefix(object, "hostapd."), MAC: strings.ToLower(mac), clientInfo: c.Clients[mac]})
		}
	}

	return s, errors.Join(errs...)
}

// speed reads a link speed as netifd prints it, e.g. 1000F: Mbit/s and duplex.
func speed(s string) (bitsPerSecond float64, duplex string, ok bool) {
	if len(s) < 2 {
		return 0, "", false
	}
	n, err := strconv.ParseFloat(s[:len(s)-1], 64)
	if err != nil || n <= 0 {
		return 0, "", false
	}
	switch s[len(s)-1] {
	case 'F':
		duplex = "full"
	case 'H':
		duplex = "half"
	default:
		return 0, "", false
	}
	return n * 1e6, duplex, true
}
