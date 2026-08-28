// Package metrics publishes what fpm-tune knows.
//
// The series are named fpm_tune_* and are deliberately disjoint from
// fpm-exporter's phpfpm_*, so both can be scraped side by side without
// collision. Running both is complementary; running only this one leaves nothing
// missing for alerting on capacity, which is the point of a tool that has to
// stand alone.
package metrics

import (
	"github.com/cboxdk/fpm-tune/plan"
	"github.com/cboxdk/fpm-tune/state"
	"github.com/prometheus/client_golang/prometheus"
)

// Collectors holds the published series.
//
// Grouped in a struct with its own registry rather than package-level globals:
// two of these can exist in one process, which is what makes them testable
// without the assertions of one test leaking into the next.
type Collectors struct {
	Registry *prometheus.Registry

	workersConfigured  *prometheus.GaugeVec
	workersRecommended *prometheus.GaugeVec
	workerRSS          *prometheus.GaugeVec
	confidence         *prometheus.GaugeVec
	demandUnmet        *prometheus.GaugeVec
	poolMeasured       *prometheus.GaugeVec

	budgetBytes       *prometheus.GaugeVec
	capacityExhausted prometheus.Gauge
	poolsUnreachable  prometheus.Gauge
	lastRun           prometheus.Gauge
}

// New builds the collectors and registers them.
func New() *Collectors {
	reg := prometheus.NewRegistry()

	c := &Collectors{
		Registry: reg,

		workersConfigured: gaugeVec(reg, "fpm_tune_pool_workers_configured",
			"pm.max_children as PHP-FPM is currently running it.", "pool"),
		workersRecommended: gaugeVec(reg, "fpm_tune_pool_workers_recommended",
			"pm.max_children fpm-tune would set. Differs from configured when a change is pending or held back by hysteresis.", "pool"),
		workerRSS: gaugeVec(reg, "fpm_tune_pool_worker_rss_bytes",
			// Labelled "estimate" rather than "quantile". Prometheus reserves
			// quantile for summary metrics, where it must be a number: PromQL's
			// histogram_quantile and most dashboard tooling treat it specially,
			// and "typical_peak" is not a quantile in any case — it is an
			// asymmetric moving estimate.
			"Learned per-worker memory. estimate=\"typical_peak\" is what sizing uses; \"high_water\" is the largest worker ever seen.", "pool", "estimate"),
		confidence: gaugeVec(reg, "fpm_tune_pool_baseline_confidence",
			"How far the learned baseline is trusted, 0 to 1. Below 1 the pool is sized from an estimate and will not be cut.", "pool"),
		demandUnmet: gaugeVec(reg, "fpm_tune_pool_demand_unmet",
			"1 when a pool wants more workers than it was given. On its own this is routine — read it with fpm_tune_capacity_exhausted.", "pool"),
		poolMeasured: gaugeVec(reg, "fpm_tune_pool_measured",
			"1 when a pool is sized from its own observed memory rather than a bootstrap estimate.", "pool"),

		budgetBytes: gaugeVec(reg, "fpm_tune_budget_bytes",
			"The memory budget, by state: total, reserved, allocated to workers, or free.", "state"),

		capacityExhausted: gauge(reg, "fpm_tune_capacity_exhausted",
			"1 when a pool wants more workers and there is nowhere left to get them — no free budget and no neighbour holding memory it is not using. "+
				"Unlike unmet demand alone, no configuration change will help: the host needs more memory or fewer pools."),
		poolsUnreachable: gauge(reg, "fpm_tune_pools_unreachable",
			"Pools that could not be scraped. Their allocation is left alone rather than handed to their neighbours."),
		lastRun: gauge(reg, "fpm_tune_last_run_timestamp_seconds",
			"When the last evaluation completed. A value that stops advancing means the loop has stalled, which no other series here would show."),
	}

	return c
}

func gaugeVec(reg *prometheus.Registry, name, help string, labels ...string) *prometheus.GaugeVec {
	v := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
	reg.MustRegister(v)

	return v
}

func gauge(reg *prometheus.Registry, name, help string) prometheus.Gauge {
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: help})
	reg.MustRegister(g)

	return g
}

// Update replaces the published values with the current evaluation.
//
// Every per-pool vector is Reset first. A pool that has been removed from the
// host would otherwise keep reporting its last value forever, and a stale
// fpm_tune_pool_demand_unmet is exactly the alert nobody can silence — it
// describes a pool that no longer exists.
func (c *Collectors) Update(result plan.Result, st *state.State, opts state.Options, now float64) {
	c.workersConfigured.Reset()
	c.workersRecommended.Reset()
	c.workerRSS.Reset()
	c.confidence.Reset()
	c.demandUnmet.Reset()
	c.poolMeasured.Reset()

	configured := map[string]int{}
	for _, v := range result.Views {
		configured[v.Name] = v.CurrentMaxChildren
	}

	for _, p := range result.Plan.Pools {
		c.workersRecommended.WithLabelValues(p.Name).Set(float64(p.MaxChildren))
		c.workersConfigured.WithLabelValues(p.Name).Set(float64(configured[p.Name]))
		c.demandUnmet.WithLabelValues(p.Name).Set(boolValue(p.DemandUnmet))
		c.poolMeasured.WithLabelValues(p.Name).Set(boolValue(p.Measured))

		var conf float64
		if st != nil {
			if ps := st.Pools[p.Name]; ps != nil {
				conf = ps.Confidence(opts)
				c.workerRSS.WithLabelValues(p.Name, "typical_peak").Set(float64(ps.TypicalPeakBytes))
				c.workerRSS.WithLabelValues(p.Name, "high_water").Set(float64(ps.HighWaterBytes))
			}
		}
		c.confidence.WithLabelValues(p.Name).Set(conf)
	}

	c.budgetBytes.WithLabelValues("total").Set(float64(result.Plan.TotalBytes))
	c.budgetBytes.WithLabelValues("reserved").Set(float64(result.Reserve))
	c.budgetBytes.WithLabelValues("allocated").Set(float64(result.Plan.AllocatedBytes))
	c.budgetBytes.WithLabelValues("free").Set(float64(result.Plan.FreeBytes))

	c.capacityExhausted.Set(boolValue(result.Plan.CapacityExhausted))
	c.poolsUnreachable.Set(float64(len(result.Unreachable)))
	c.lastRun.Set(now)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}

	return 0
}
