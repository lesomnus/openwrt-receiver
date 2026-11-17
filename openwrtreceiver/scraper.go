package openwrtreceiver

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/scraper/scrapererror"
	"go.uber.org/zap"
)

// newRunner finds the ubus tool; tests stand in for it.
var newRunner = execRunner

type openwrtScraper struct {
	cfg    *Config
	logger *zap.Logger

	r           *reader
	unavailable bool
	starts      *starts
}

func newScraper(cfg *Config, logger *zap.Logger) *openwrtScraper {
	return &openwrtScraper{cfg: cfg, logger: logger}
}

func (s *openwrtScraper) start(_ context.Context, _ component.Host) error {
	s.starts = newStarts(pcommon.NewTimestampFromTime(time.Now()))
	return nil
}

func (s *openwrtScraper) scrape(ctx context.Context) (pmetric.Metrics, error) {
	if s.r == nil {
		run, err := newRunner(s.cfg.Ubus)
		if err != nil {
			if errors.Is(err, errNotAvailable) {
				// One config fits every node: where there is no ubus, say so once and stay idle.
				if !s.unavailable {
					s.logger.Info("ubus is not available; the receiver stays idle", zap.Error(err))
					s.unavailable = true
				}
				return pmetric.NewMetrics(), nil
			}
			return pmetric.NewMetrics(), err
		}
		if s.unavailable {
			s.logger.Info("ubus is available")
			s.unavailable = false
		}
		s.r = &reader{run: run, sysRoot: s.cfg.RootPath, leaseFile: s.cfg.LeaseFile}
	}

	snap, err := s.r.read(ctx)
	md := toMetrics(collect(snap), s.starts, pcommon.NewTimestampFromTime(snap.time))
	if err != nil {
		failed := 1
		if j, ok := err.(interface{ Unwrap() []error }); ok {
			failed = len(j.Unwrap())
		}
		return md, scrapererror.NewPartialScrapeError(err, failed)
	}
	return md, nil
}
