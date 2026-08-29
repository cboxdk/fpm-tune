package serve

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/state"
	"github.com/cboxdk/phpfpm"
)

// TestARememberedMasterComesBackWithoutAPID.
//
// The rule the code states and nothing checked: when no master is running but a
// previous run recorded where one lives, the Master is returned with its paths
// and a PID of ZERO — there is nothing to signal, and a caller that reads a
// non-zero pid as "there is a master here" would try to reload a process that
// does not exist.
//
// It is the same confusion apply guards one layer down, and having it guarded in
// exactly one of the two places is how it comes back.
func TestARememberedMasterComesBackWithoutAPID(t *testing.T) {
	defer noMastersRunning()()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "php-fpm.conf")
	if err := os.WriteFile(configPath,
		[]byte("[global]\ninclude = "+dir+"/pool.d/*.conf\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := MasterFromMemory("", state.MasterRef{
		Binary: "/usr/sbin/php-fpm", ConfigPath: configPath,
		DropInDir: filepath.Join(dir, "pool.d"),
	}, nil)
	if err != nil {
		t.Fatalf("MasterFromMemory: %v", err)
	}

	if m.PID != 0 {
		t.Errorf("PID = %d for a master that is not running: a caller reading that as a "+
			"live master signals a pid the kernel may have given to something else", m.PID)
	}
	if m.ConfigPath != configPath {
		t.Errorf("ConfigPath = %q, want the remembered one", m.ConfigPath)
	}
	if len(m.IncludePatterns) == 0 {
		t.Error("no include patterns were read back from the remembered config; without " +
			"them the check that the drop-in directory is actually included cannot run")
	}
}

// TestNoMasterAndNothingRememberedIsRefused: the other half, so a function that
// returned a zero Master for everything could not pass the test above.
func TestNoMasterAndNothingRememberedIsRefused(t *testing.T) {
	defer noMastersRunning()()

	if _, err := MasterOnHost("", nil); !errors.Is(err, ErrNoMaster) {
		t.Errorf("err = %v, want ErrNoMaster", err)
	}
}

// TestTheDropInDirectoryPicksTheMasterOut.
//
// On a host running two PHP versions the pools belong to both masters and the
// memory limit belongs to one of them, so planning them together is incoherent.
// The directory is how an operator says which one they mean, and the filter that
// honours it was covered by nothing.
func TestTheDropInDirectoryPicksTheMasterOut(t *testing.T) {
	dir := t.TempDir()
	eight := writeMasterConfig(t, dir, "8.2")
	five := writeMasterConfig(t, dir, "8.5")

	defer swapDiscovery([]phpfpm.Master{
		{PID: 100, ConfigPath: eight, Binary: "/usr/sbin/php-fpm8.2"},
		{PID: 200, ConfigPath: five, Binary: "/usr/sbin/php-fpm8.5"},
	})()

	m, err := MasterOnHost(filepath.Join(dir, "8.5", "pool.d"), nil)
	if err != nil {
		t.Fatalf("MasterOnHost: %v", err)
	}
	if m.PID != 200 {
		t.Errorf("pid %d was picked for the 8.5 pool directory; the plan would be sized "+
			"against the other master's memory limit", m.PID)
	}
}

// TestTwoMastersAndNoDirectoryIsRefused: without the directory there is no
// answer, and picking one would silently size every pool on the host against one
// of the two limits.
func TestTwoMastersAndNoDirectoryIsRefused(t *testing.T) {
	dir := t.TempDir()

	defer swapDiscovery([]phpfpm.Master{
		{PID: 100, ConfigPath: writeMasterConfig(t, dir, "8.2")},
		{PID: 200, ConfigPath: writeMasterConfig(t, dir, "8.5")},
	})()

	_, err := MasterOnHost("", nil)
	if err == nil {
		t.Fatal("a host running two masters was planned as one")
	}
	// The message has to carry both, or the operator cannot answer it.
	for _, want := range []string{"pid 100", "pid 200"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s:\n%v", want, err)
		}
	}
}

// TestMasterPIDOfTakesTheFirstOneKnown: the budget is read from the MASTER's
// cgroup, so a views slice whose first entries failed to scrape must not yield
// zero — that reads the root cgroup, which on a VM is the whole machine.
func TestMasterPIDOfTakesTheFirstOneKnown(t *testing.T) {
	got := MasterPIDOf([]observe.PoolView{
		{Name: "down"},
		{Name: "up", Target: phpfpm.Target{PID: 4242}},
	})
	if got != 4242 {
		t.Errorf("MasterPIDOf = %d, want 4242: a pool that could not be scraped still "+
			"belongs to a master, and sizing against the root cgroup on a VM finds the "+
			"machine rather than php-fpm's own limit", got)
	}
}

func writeMasterConfig(t *testing.T, root, version string) string {
	t.Helper()

	dir := filepath.Join(root, version)
	if err := os.MkdirAll(filepath.Join(dir, "pool.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "php-fpm.conf")
	body := "[global]\ninclude = " + filepath.Join(dir, "pool.d", "*.conf") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}

func swapDiscovery(masters []phpfpm.Master) func() {
	saved := discoverMasters
	discoverMasters = func(*slog.Logger) ([]phpfpm.Master, error) { return masters, nil }

	return func() { discoverMasters = saved }
}

func noMastersRunning() func() { return swapDiscovery(nil) }

// TestTheDaemonPlansOnlyTheMasterItWasPointedAt.
//
// --drop-in-dir "also selects which master to manage on a host running several",
// says its own help — and the filter that honours it lived in the CLI, so plan
// and apply scoped and serve did not.
//
// A daemon on a two-master host therefore learned, sized and RENDERED the other
// master's pools. The budget came from whichever master owned the first pool
// alphabetically, so the plan was divided against a cgroup limit binding nothing
// it was writing for; and the rendered file named pools that master does not
// serve, which the sandbox refused every round — the safety net working, and a
// daemon that never applies anything again.
func TestTheDaemonPlansOnlyTheMasterItWasPointedAt(t *testing.T) {
	dir := t.TempDir()
	mine := writeMasterConfig(t, dir, "8.5")
	theirs := writeMasterConfig(t, dir, "8.2")

	targets := []phpfpm.Target{
		{Name: "shop", ConfigPath: mine, PID: 100},
		{Name: "api", ConfigPath: theirs, PID: 200},
	}

	kept := ForMaster(targets, filepath.Join(dir, "8.5", "pool.d"), nil)

	if len(kept) != 1 || kept[0].Name != "shop" {
		names := make([]string, 0, len(kept))
		for _, k := range kept {
			names = append(names, k.Name)
		}
		t.Errorf("planning %v for a daemon pointed at the 8.5 pool directory; the other "+
			"master's pools are sized against a limit that does not bind them, and "+
			"written into a file it does not read", names)
	}
}

// TestNoMasterIsChosenWhenThePoolsSpanTwo: the budget is read from ONE master's
// cgroup, so views spanning two masters have no single right answer. Returning
// the first pool's pid picks one host's limit to size another host's pools
// against — the same fault as reading the root cgroup, reached from the other
// side. Zero means "no opinion", and the caller falls back to the machine.
func TestNoMasterIsChosenWhenThePoolsSpanTwo(t *testing.T) {
	if got := MasterPIDOf([]observe.PoolView{
		{Name: "a", Target: phpfpm.Target{PID: 100}},
		{Name: "b", Target: phpfpm.Target{PID: 200}},
	}); got != 0 {
		t.Errorf("MasterPIDOf = %d for pools belonging to two different masters; that "+
			"master's memory limit is about to be divided among pools it does not run",
			got)
	}

	// One master, some pools unreachable: still an answer.
	if got := MasterPIDOf([]observe.PoolView{
		{Name: "down"},
		{Name: "up", Target: phpfpm.Target{PID: 4242}},
		{Name: "also-up", Target: phpfpm.Target{PID: 4242}},
	}); got != 4242 {
		t.Errorf("MasterPIDOf = %d, want 4242", got)
	}
}

// TestARememberedMasterIsNotReusedForAnotherDirectory.
//
// The remembered reference describes ONE master, and a caller naming a
// different pool directory is asking about a different one. It used to keep the
// remembered binary and config and overwrite only the directory — so on a host
// where 8.3 applied last and 8.2 is down, a repair for 8.2 was handed 8.3's
// php-fpm and its config, validated a tree it was not about to touch, found it
// fine, and left the broken host alone.
//
// Same fault as a shared sidecar, one layer up, and this layer is consulted
// first.
func TestARememberedMasterIsNotReusedForAnotherDirectory(t *testing.T) {
	defer noMastersRunning()()

	dir := t.TempDir()
	applied := writeMasterConfig(t, dir, "8.3")
	remembered := state.MasterRef{
		Binary: "/usr/sbin/php-fpm8.3", ConfigPath: applied,
		DropInDir: filepath.Join(dir, "8.3", "pool.d"),
	}

	// Asking about the OTHER master.
	other := writeMasterConfig(t, dir, "8.2")
	_ = other
	if _, err := MasterFromMemory(filepath.Join(dir, "8.2", "pool.d"), remembered, nil); err == nil {
		t.Error("a repair for 8.2 was handed 8.3's php-fpm; it validates a tree it is not " +
			"about to touch, finds it fine, and leaves the broken host alone")
	}

	// And its own directory still resolves.
	m, err := MasterFromMemory(remembered.DropInDir, remembered, nil)
	if err != nil {
		t.Fatalf("the master's own directory was refused: %v", err)
	}
	if m.Binary != remembered.Binary {
		t.Errorf("Binary = %q, want the remembered one", m.Binary)
	}
}
