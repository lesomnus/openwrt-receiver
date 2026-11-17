package openwrtreceiver

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.opentelemetry.io/collector/scraper/scraperhelper"
)

type Config struct {
	scraperhelper.ControllerConfig `mapstructure:",squash"`

	// The ubus tool, which every call goes through, e.g. `ubus call network.device status`.
	Ubus string `mapstructure:"ubus"`
	// Reads `<root_path>/sys` instead of `/sys`, like `hostmetrics`.
	RootPath string `mapstructure:"root_path"`
	// dnsmasq's lease file: one line per DHCP lease.
	LeaseFile string `mapstructure:"lease_file"`
}

func (c *Config) Validate() error {
	if c.Ubus == "" {
		return errors.New(`"ubus" must not be empty`)
	}
	if c.RootPath == "" {
		return nil
	}
	if !filepath.IsAbs(c.RootPath) {
		return fmt.Errorf(`"root_path" must be absolute: %q`, c.RootPath)
	}
	info, err := os.Stat(c.RootPath)
	if err != nil {
		return fmt.Errorf(`"root_path": %w`, err)
	}
	if !info.IsDir() {
		return errors.New(`"root_path" is not a directory`)
	}
	return nil
}
