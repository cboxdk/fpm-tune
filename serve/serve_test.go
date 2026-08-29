package serve

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cboxdk/fpm-tune/allocate"
	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/plan"
	"github.com/cboxdk/fpm-tune/state"
	"github.com/cboxdk/phpfpm"
)

// TestIncludeDirIsReadNotGuessed: the layouts genuinely differ. RHEL puts the
// master at /etc/php-fpm.conf including /etc/php-fpm.d/*.conf; Debian uses
// /etc/php/8.2/fpm/php-fpm.conf including .../pool.d/*.conf. Deriving the
// directory from the master's own location would be wrong on one of them, and
// writing a pool fragment where PHP-FPM does not read it is a silent no-op — the
// worst kind of failure for a tool like this.
func TestIncludeDirIsReadNotGuessed(t *testing.T) {
	tests := map[string]struct {
		config string
		want   string
	}{
		"rhel layout": {
			config: "[global]\npid = /run/php-fpm.pid\n\ninclude=/etc/php-fpm.d/*.conf\n",
			want:   "/etc/php-fpm.d",
		},
		"debian layout": {
			config: "[global]\n\ninclude=/etc/php/8.2/fpm/pool.d/*.conf\n",
			want:   "/etc/php/8.2/fpm/pool.d",
		},
		"spaces around the equals": {
			config: "include = /etc/php-fpm.d/*.conf\n",
			want:   "/etc/php-fpm.d",
		},
		"commented out": {
			config: "; include=/etc/wrong.d/*.conf\ninclude=/etc/right.d/*.conf\n",
			want:   "/etc/right.d",
		},
		"no include at all": {
			config: "[global]\npid = /run/php-fpm.pid\n",
			want:   "",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "php-fpm.conf")
			if err := os.WriteFile(path, []byte(tt.config), 0o600); err != nil {
				t.Fatal(err)
			}

			if got := IncludeDirOf(path); got != tt.want {
				t.Errorf("IncludeDirOf = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRelativeIncludeIsResolved against the master config's own directory,
// which is how PHP-FPM reads it.
func TestRelativeIncludeIsResolved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "php-fpm.conf")
	if err := os.WriteFile(path, []byte("include=pool.d/*.conf\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(dir, "pool.d")
	if got := IncludeDirOf(path); got != want {
		t.Errorf("IncludeDirOf = %q, want %q", got, want)
	}
}

// TestPIDFileIsRead so a reload can go to the authoritative pid rather than
// whatever the process table suggests.
func TestPIDFileIsRead(t *testing.T) {
	tests := map[string]struct {
		config string
		want   string
	}{
		"present":     {"[global]\npid = /run/php-fpm.pid\n", "/run/php-fpm.pid"},
		"no spaces":   {"pid=/var/run/php.pid\n", "/var/run/php.pid"},
		"commented":   {"; pid = /run/wrong.pid\npid = /run/right.pid\n", "/run/right.pid"},
		"absent":      {"[global]\ndaemonize = no\n", ""},
		"not the key": {"; something about pid\nrapid = no\n", ""},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "php-fpm.conf")
			if err := os.WriteFile(path, []byte(tt.config), 0o600); err != nil {
				t.Fatal(err)
			}

			if got := PIDFileOf(path); got != tt.want {
				t.Errorf("PIDFileOf = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestTwoMastersAreRefusedRatherThanHalfHandled: reconfiguring one and silently
// ignoring the other would leave the operator with a host that is half tuned and
// no indication of which half.
func TestTwoMastersAreRefusedRatherThanHalfHandled(t *testing.T) {
	_, err := MasterFrom(plan.Result{Views: []observe.PoolView{
		{Name: "a", Target: phpfpm.Target{Binary: "/usr/sbin/php-fpm8.2", ConfigPath: "/etc/php/8.2/fpm/php-fpm.conf"}},
		{Name: "b", Target: phpfpm.Target{Binary: "/usr/sbin/php-fpm8.3", ConfigPath: "/etc/php/8.3/fpm/php-fpm.conf"}},
	}}, "")

	if err == nil {
		t.Fatal("two masters were accepted; one would have been silently ignored")
	}
	if !strings.Contains(err.Error(), "2 PHP-FPM masters") {
		t.Errorf("the error does not say what the problem is: %v", err)
	}
}

// TestOneMasterResolves, including the include directory read from its config.
func TestOneMasterResolves(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "php-fpm.conf")
	poolDir := filepath.Join(dir, "php-fpm.d")
	if err := os.WriteFile(config, []byte("include="+poolDir+"/*.conf\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	master, err := MasterFrom(plan.Result{Views: []observe.PoolView{
		{Name: "a", Target: phpfpm.Target{Binary: "/usr/sbin/php-fpm", ConfigPath: config}},
		{Name: "b", Target: phpfpm.Target{Binary: "/usr/sbin/php-fpm", ConfigPath: config}},
	}}, "")
	if err != nil {
		t.Fatalf("MasterFrom: %v", err)
	}

	if master.DropInDir != poolDir {
		t.Errorf("DropInDir = %q, want %q", master.DropInDir, poolDir)
	}
	if master.ConfigPath != config {
		t.Errorf("ConfigPath = %q, want %q", master.ConfigPath, config)
	}
}

// TestNoMasterIsAClearError rather than a nil struct that fails later.
func TestNoMasterIsAClearError(t *testing.T) {
	if _, err := MasterFrom(plan.Result{}, ""); err == nil {
		t.Error("a plan with no pools produced a usable master")
	}
}

// TestStateSurvivesShutdown. A daemon that discards an hour of observation
// because it was asked to stop makes every restart a return to bootstrap, which
// is the behaviour persisting state exists to avoid.
func TestStateSurvivesShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	loop, err := New(Config{StatePath: path, MetricsAddr: ""}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	loop.State().Learn(state.Observation{
		Pool: "shop", At: time.Now(),
		Workers: []state.WorkerSample{
			{RSSBytes: 80 << 20, Requests: 400},
			{RSSBytes: 90 << 20, Requests: 400},
		},
	}, state.Options{})

	loop.shutdown(nil)

	reloaded, err := state.Load(path)
	if err != nil {
		t.Fatalf("Load after shutdown: %v", err)
	}
	ps := reloaded.Pools["shop"]
	if ps == nil {
		t.Fatal("the observation was lost on shutdown")
	}
	if ps.TypicalPeakBytes != 90<<20 {
		t.Errorf("baseline = %d, want the 90MB peak", ps.TypicalPeakBytes>>20)
	}
}

// TestNewSurfacesACorruptStateFile rather than starting over silently, which
// would re-tune the host from bootstrap estimates while looking healthy.
func TestNewSurfacesACorruptStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := New(Config{StatePath: path}, nil); err == nil {
		t.Error("a corrupt state file was accepted silently")
	}
}

// TestRunStopsOnContextCancel and does not hang on the ticker.
func TestRunStopsOnContextCancel(t *testing.T) {
	loop, err := New(Config{
		StatePath:   filepath.Join(t.TempDir(), "state.json"),
		Interval:    time.Hour, // never fires; cancellation is what must end it
		MetricsAddr: "",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx) }()

	// The first round runs immediately and finds no pools on this host, which is
	// fine — it must not be fatal.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop within 10s of cancellation")
	}
}

// TestSaveIsThrottledButForceable: learning happens every interval, and writing
// a file that often for the life of a host is a lot of writes for data only read
// at startup. A change worth remembering bypasses the throttle.
func TestSaveIsThrottledButForceable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	loop, err := New(Config{StatePath: path, SaveEvery: time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()

	loop.save(now, false)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("the first save did not happen")
	}
	first := modTime(t, path)

	// Too soon, and nothing important happened.
	time.Sleep(20 * time.Millisecond)
	loop.save(now.Add(time.Second), false)
	if !modTime(t, path).Equal(first) {
		t.Error("state was rewritten inside the save interval")
	}

	// Something happened that a restart must not lose.
	time.Sleep(20 * time.Millisecond)
	loop.save(now.Add(2*time.Second), true)
	if modTime(t, path).Equal(first) {
		t.Error("a forced save did not write")
	}
}

// TestMetricsEndpointIsReachable.
//
// Dialled, because the point of the endpoint is that something else can reach
// it. The previous version of this test bound nothing and called Registry.Gather
// directly, which is the metrics package's own test — re-routing /metrics to
// /nope left it green, and so did downgrading the bind failure from an error to
// a log line.
//
// That second one matters more than it looks: a metrics server that could not
// bind and carried on is a daemon that looks alive and publishes nothing, which
// is also the situation where two of them are writing the same pool files.
func TestMetricsEndpointIsReachable(t *testing.T) {
	loop, err := New(Config{
		StatePath:   filepath.Join(t.TempDir(), "state.json"),
		MetricsAddr: "127.0.0.1:0",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	loop.Metrics().Update(plan.Result{
		Plan: allocate.Plan{Pools: []allocate.PoolPlan{{Name: "shop", MaxChildren: 8}}},
	}, loop.State(), state.Options{}, 1)

	srv, err := loop.startMetrics()
	if err != nil {
		t.Fatalf("startMetrics: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	base := "http://" + loop.BoundMetricsAddr()

	body := get(t, base+"/metrics")
	if !strings.Contains(body, "fpm_tune_pool_workers_recommended") {
		t.Errorf("/metrics does not carry the series a scraper comes for:\n%s", body)
	}
	if got := get(t, base+"/healthz"); !strings.Contains(got, "ok") {
		t.Errorf("/healthz = %q", got)
	}

	// A second loop asked for the SAME address must refuse to start.
	second, err := New(Config{
		StatePath:   filepath.Join(t.TempDir(), "state.json"),
		MetricsAddr: loop.BoundMetricsAddr(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if srv2, err := second.startMetrics(); err == nil {
		_ = srv2.Close()
		t.Error("a second process bound the same metrics address without complaint; a " +
			"daemon that could not serve its metrics and carried on looks alive and " +
			"publishes nothing")
	}
}

func get(t *testing.T, url string) string {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return string(body)
}

// TestMetricsRegistryCarriesTheSeries is the handler-free half, kept because it
// runs where a listener cannot be bound.
func TestMetricsRegistryCarriesTheSeries(t *testing.T) {
	loop, err := New(Config{
		StatePath:   filepath.Join(t.TempDir(), "state.json"),
		MetricsAddr: "127.0.0.1:0", // not dialled; the handler is what matters
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	loop.Metrics().Update(plan.Result{
		Plan: allocate.Plan{Pools: []allocate.PoolPlan{{Name: "shop", MaxChildren: 8}}},
	}, loop.State(), state.Options{}, 1)

	families, err := loop.Metrics().Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if len(families) == 0 {
		t.Fatal("nothing is published")
	}

	var found bool
	for _, f := range families {
		if f.GetName() == "fpm_tune_pool_workers_recommended" {
			found = true
		}
	}
	if !found {
		t.Error("fpm_tune_pool_workers_recommended is not published")
	}
}

func modTime(t *testing.T, path string) time.Time {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	return info.ModTime()
}

// TestIncludePathIsNotAnInclude: the match was a prefix, so an include_path
// directive above the real include won. Pool fragments then went to
// /usr/share/php, which php-fpm never reads — a silent no-op, the worst failure
// shape for a tool that reports success.
func TestIncludePathIsNotAnInclude(t *testing.T) {
	path := filepath.Join(t.TempDir(), "php-fpm.conf")
	body := "[global]\ninclude_path = /usr/share/php/lib\ninclude = /etc/php-fpm.d/*.conf\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := IncludeDirOf(path); got != "/etc/php-fpm.d" {
		t.Errorf("IncludeDirOf = %q, want /etc/php-fpm.d", got)
	}
}
