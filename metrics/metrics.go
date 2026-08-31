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
	subtreeRSS         *prometheus.GaugeVec
	childRSS           *prometheus.GaugeVec
	confidence         *prometheus.GaugeVec
	demandUnmet        *prometheus.GaugeVec
	poolMeasured       *prometheus.GaugeVec

	poolsAmbiguous    prometheus.Gauge
	budgetBytes       *prometheus.GaugeVec
	cgroupBytes       *prometheus.GaugeVec
	capacityExhausted prometheus.Gauge
	poolsUnreachable  prometheus.Gauge
	lastRun           prometheus.Gauge

	applyEnabled  prometheus.Gauge
	applyBlocked  *prometheus.GaugeVec
	rollbackFail  prometheus.Counter
	lastApply     prometheus.Gauge
	appliesFailed prometheus.Counter
	rollbacks     prometheus.Counter
	repairs       prometheus.Counter
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
			"Per-worker memory, read as PSS where the kernel reports it (shared opcache and libraries divided among sharers rather than charged in full to each worker) and RSS otherwise, so it reads below what top shows. estimate=\"typical_peak\" is what sizing uses; \"high_water\" is the largest worker ever seen; \"p50\"/\"p95\"/\"p99\" are the measured spread, which is what to graph when a host misbehaves at its busiest minute rather than on average.", "pool", "estimate"),
		subtreeRSS: gaugeVec(reg, "fpm_tune_pool_subtree_rss_bytes",
			"The high-water mark of a worker AND everything it spawned — an ffmpeg, an imagemagick — which fpm_tune_pool_worker_rss_bytes does not include. Compare it to that metric's high_water: the gap is what children cost. A point-in-time sample misses a child that lived and died between scrapes; the cgroup high-water, where there is a cgroup, is what catches those.", "pool"),
		childRSS: gaugeVec(reg, "fpm_tune_pool_child_rss_bytes",
			"The child memory folded into each worker's cost: the high-water of a scrape's total spawned-child memory divided by its workers, so it already reflects how many workers ran a child at once. Zero for a plain web pool, large for one doing media work. Multiply by pm.max_children for what the pool commits to children.", "pool"),
		confidence: gaugeVec(reg, "fpm_tune_pool_baseline_confidence",
			// Corrected: it used to say a pool below 1 is "sized from an
			// estimate", which stopped being true when the cost and the
			// permission to shrink were separated. A measurement is a
			// measurement whatever the confidence.
			"How far the learned baseline is trusted, 0 to 1. Below 1 the pool will not be cut below what it is configured for; its per-worker cost is still whatever has been measured. See fpm_tune_pool_measured.", "pool"),
		demandUnmet: gaugeVec(reg, "fpm_tune_pool_demand_unmet",
			"1 when a pool wants more workers than it was given, and could not be. fpm_tune_capacity_exhausted is 1 whenever any pool is in this state — the two are the same news at different granularity, which pool and whether any.", "pool"),
		poolMeasured: gaugeVec(reg, "fpm_tune_pool_measured",
			"1 when a pool is sized from its own observed memory rather than a bootstrap estimate.", "pool"),

		poolsAmbiguous: gauge(reg, "fpm_tune_pools_ambiguous",
			"How many pool names are shared by more than one PHP-FPM master this round. Above zero, those pools are NOT published: every per-pool series is labelled by name, so two pools called www would set the same series twice and the endpoint would report whichever ran last. Name a pool directory with --drop-in-dir."),

		budgetBytes: gaugeVec(reg, "fpm_tune_budget_bytes",
			"The memory budget, by state: total, reserved, allocated to workers, or free.", "state"),

		cgroupBytes: gaugeVec(reg, "fpm_tune_cgroup_memory_bytes",
			"What the master's cgroup has actually used, by state: \"current\" now, \"peak\" at its high-water. Counts every process in the cgroup — workers AND the children they spawned — so it is the ground truth the OOM killer enforces against, and the number to compare fpm_tune_budget_bytes against. Absent on a host with no cgroup, where the per-pool subtree metrics are the only view.", "state"),

		capacityExhausted: gauge(reg, "fpm_tune_capacity_exhausted",
			"1 when a pool wants more workers and there is nowhere left to get them — no free budget and no neighbour holding memory it is not using. "+
				"Unlike unmet demand alone, no configuration change will help: the host needs more memory or fewer pools."),
		applyEnabled: gauge(reg, "fpm_tune_apply_enabled",
			"1 when this process will act on its plan. A tool that only observes looks "+
				"identical to one that is acting, and the difference is the whole question "+
				"when a host is not being tuned."),
		applyBlocked: gaugeVec(reg, "fpm_tune_apply_blocked",
			"1 when a round declined to apply for a reason other than having nothing to "+
				"do. fpm_tune_apply_enabled says what this process was ASKED to do and "+
				"never changes; this says whether it can, which is the question when a "+
				"host is not being tuned.", "reason"),
		rollbackFail: counter(reg, "fpm_tune_rollback_failed_total",
			"Changes php-fpm rejected that could NOT be taken back out. Strictly worse "+
				"than a rollback and previously invisible: nothing is broken yet, because "+
				"the master was never signalled, but the next reload from any source "+
				"adopts what is on disk. Alert on any increase."),
		lastApply: gauge(reg, "fpm_tune_last_apply_timestamp_seconds",
			"When a change was last written and adopted. Distinct from the evaluation "+
				"timestamp: a loop can be running and deciding to do nothing, which is "+
				"correct, or running and failing to act, which is not."),
		appliesFailed: counter(reg, "fpm_tune_applies_failed_total",
			"Applies that ended in an error. The log says what; this says how often, "+
				"which is what an alert needs."),
		rollbacks: counter(reg, "fpm_tune_rollbacks_total",
			"Changes taken back out because php-fpm rejected them or its master did not "+
				"survive the reload. Any value above zero deserves a look at the log."),
		repairs: counter(reg, "fpm_tune_repairs_total",
			"Times this tool had to undo or complete something a previous run left behind, "+
				"or remove its own file to let php-fpm start."),

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

func counter(reg *prometheus.Registry, name, help string) prometheus.Counter {
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: name, Help: help})
	reg.MustRegister(c)

	return c
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
	// Which master each pool belongs to. A pool name is not an identity — `www`
	// is the default in every distribution's package — so looking a baseline up
	// by name alone reports another master's confidence and another master's
	// measured cost under this one's label.
	masters := make(map[string]string, len(result.Views))
	for _, v := range result.Views {
		masters[v.Name] = v.Target.ConfigPath
	}

	// Pools whose NAME is shared by another master this round are not published
	// at all.
	//
	// Every per-pool series is labelled by name, so two pools called `www` set
	// the same series twice and the endpoint reports whichever plan row ran
	// last — silently, and differently on each scrape. A missing series is
	// visible; a wrong one is not. The count below is what says why.
	ambiguous := make(map[string]bool, len(result.Ambiguous))
	for _, name := range result.Ambiguous {
		ambiguous[name] = true
	}
	c.poolsAmbiguous.Set(float64(len(result.Ambiguous)))

	c.workersConfigured.Reset()
	c.workersRecommended.Reset()
	c.workerRSS.Reset()
	c.subtreeRSS.Reset()
	c.childRSS.Reset()
	c.confidence.Reset()
	c.demandUnmet.Reset()
	c.poolMeasured.Reset()

	configured := map[string]int{}
	for _, v := range result.Views {
		configured[v.Name] = v.CurrentMaxChildren
	}

	for _, p := range result.Plan.Pools {
		if ambiguous[p.Name] {
			continue
		}

		c.workersRecommended.WithLabelValues(p.Name).Set(float64(p.MaxChildren))
		c.workersConfigured.WithLabelValues(p.Name).Set(float64(configured[p.Name]))
		c.demandUnmet.WithLabelValues(p.Name).Set(boolValue(p.DemandUnmet))
		c.poolMeasured.WithLabelValues(p.Name).Set(boolValue(p.Measured))

		var conf float64
		if st != nil {
			if ps := st.Lookup(masters[p.Name], p.Name); ps != nil {
				// The spread as well as the estimate. Sizing follows typical_peak;
				// these say what the workers actually measured, which is what an
				// advisory deployment is for and what a dashboard wants when a host
				// misbehaves at its busiest minute rather than on average.
				if ps.RSSSamples > 0 {
					c.workerRSS.WithLabelValues(p.Name, "p50").Set(float64(ps.Percentile(0.50)))
					c.workerRSS.WithLabelValues(p.Name, "p95").Set(float64(ps.Percentile(0.95)))
					c.workerRSS.WithLabelValues(p.Name, "p99").Set(float64(ps.Percentile(0.99)))
				}
				conf = ps.Confidence(opts)
				c.workerRSS.WithLabelValues(p.Name, "typical_peak").Set(float64(ps.TypicalPeakBytes))
				c.workerRSS.WithLabelValues(p.Name, "high_water").Set(float64(ps.HighWaterBytes))

				// Only once a subtree has actually been measured, so a pool on an
				// older scrape (or one whose process table could not be read) does
				// not publish a zero that reads as "children cost nothing".
				if ps.SubtreeHighWaterBytes > 0 {
					c.subtreeRSS.WithLabelValues(p.Name).Set(float64(ps.SubtreeHighWaterBytes))
					c.childRSS.WithLabelValues(p.Name).Set(float64(ps.ChildPerWorkerHighWaterBytes))
				}
			}
		}
		c.confidence.WithLabelValues(p.Name).Set(conf)
	}

	c.budgetBytes.WithLabelValues("total").Set(float64(result.Plan.TotalBytes))
	c.budgetBytes.WithLabelValues("reserved").Set(float64(result.Reserve))
	// The child reserve as its own series, so a dashboard can show how much of a
	// host is being kept for spawned processes rather than workers. Zero, and
	// harmlessly present, on a host whose pools spawn nothing.
	c.budgetBytes.WithLabelValues("reserved_children").Set(float64(result.ChildReserve))
	c.budgetBytes.WithLabelValues("allocated").Set(float64(result.Plan.AllocatedBytes))
	c.budgetBytes.WithLabelValues("free").Set(float64(result.Plan.FreeBytes))

	// Cleared and set only when there is a cgroup, so a bare VM does not publish
	// a zero that reads as "the cgroup used no memory" rather than "there is no
	// cgroup". The per-pool subtree metrics carry the child memory there.
	c.cgroupBytes.Reset()
	if result.HasCgroupUsage {
		c.cgroupBytes.WithLabelValues("current").Set(float64(result.CgroupUsage.CurrentBytes))
		c.cgroupBytes.WithLabelValues("peak").Set(float64(result.CgroupUsage.PeakBytes))
	}

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

// SetApplyEnabled records whether this process acts on its plans.
func (c *Collectors) SetApplyEnabled(on bool) {
	v := 0.0
	if on {
		v = 1
	}
	c.applyEnabled.Set(v)
}

// RecordApply notes the outcome of one apply.
//
// Counted rather than logged alone, because the log now reports a persistent
// condition once rather than every interval — which is right for reading and
// useless for alerting. A counter is the thing to alert on.
func (c *Collectors) RecordApply(at float64, changed, rolledBack bool, rollbackFailed int, err error) {
	switch {
	case err != nil:
		c.appliesFailed.Inc()
	case changed:
		c.lastApply.Set(at)
	}
	if rolledBack {
		c.rollbacks.Inc()
	}
	if rollbackFailed > 0 {
		c.rollbackFail.Inc()
	}
}

// SetApplyBlocked records why a round could not apply, or clears it.
//
// Separate from SetApplyEnabled, which reflects the flag and never changes. A
// daemon that cannot reconcile, cannot take the lock or cannot identify the
// master applies nothing for the life of the process while reporting itself
// enabled — answering the one question it exists to answer wrongly, in the
// failure mode where it is asked.
func (c *Collectors) SetApplyBlocked(reason string) {
	c.applyBlocked.Reset()
	if reason != "" {
		c.applyBlocked.WithLabelValues(reason).Set(1)
	}
}

// RecordRepair notes that recovery had to act.
func (c *Collectors) RecordRepair() { c.repairs.Inc() }
