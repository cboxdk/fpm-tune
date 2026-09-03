package serve

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cboxdk/phpfpm"
)

// shortDir is a directory under /tmp for a unix socket: the path is limited to
// about a hundred characters, and macOS's t.TempDir is longer than that.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fpmt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return dir
}

// runLoop starts Run in a goroutine and returns a stop that cancels it and
// waits for it, asserting it returned nil. The test's cleanup stops it too,
// so a failed assertion does not leave Run writing into a directory the
// test is removing.
func runLoop(t *testing.T, loop *Loop) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Run returned %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Run did not stop within 10s of cancellation")
			}
		})
	}
	t.Cleanup(stop)

	return stop
}

// waitForSocket waits for Run to have created the control socket at path.
func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no control socket at %s within 5s", path)
}

// TestAnApplyNowOnAWatchingDaemonRunsOneRoundAndLetsGo: the Run loop's
// applyNow arm runs one round with applying forced on, answers with what the
// round did — here, on a host with no pools, the default "did not reach the
// plan" — and, because the daemon is watching rather than applying, gives the
// pool directory back afterwards: the lock is released, the reconciled flag
// is cleared with it (the next forced round reconciles before writing), the
// forced flag is off, and no apply_blocked reason is left published for a
// daemon that is not blocked from applying, it simply does not. Cancelling
// the context afterwards still ends Run cleanly.
func TestAnApplyNowOnAWatchingDaemonRunsOneRoundAndLetsGo(t *testing.T) {
	dir := shortDir(t)
	loop, err := New(Config{
		StatePath:   filepath.Join(t.TempDir(), "state.json"),
		Interval:    time.Hour, // never fires; the apply-now and the cancel drive it
		ControlPath: dir + "/control.sock",
		DropInDir:   t.TempDir(),
		BackupDir:   t.TempDir(),
		Discover:    func(context.Context) ([]phpfpm.Target, error) { return nil, nil },
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stop := runLoop(t, loop)
	waitForSocket(t, loop.cfg.ControlPath)

	ctx, cancelReq := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelReq()
	out, err := ApplyNow(ctx, loop.cfg.ControlPath)
	if err != nil {
		t.Fatal(err)
	}
	const want = "nothing was applied: the round did not reach the plan"
	if out.Message != want || out.Error != "" || len(out.Changed) != 0 {
		t.Errorf("outcome = %+v, want the message %q and nothing else", out, want)
	}

	// The reply is sent after the round's cleanup, so by now the loop is back
	// at the select and its fields are quiescent.
	if loop.forceApply {
		t.Error("forceApply is still on after the forced round")
	}
	if loop.resource != nil {
		t.Error("a watching daemon kept the pool-directory lock after its one write")
	}
	if loop.reconciled {
		t.Error("reconciled survived the release of the lock; the next forced round must reconcile before writing")
	}
	if got := blockedReason(t, loop); got != "" {
		t.Errorf("apply_blocked = %q after an apply-now on a watching daemon, want none", got)
	}

	stop()
	if _, err := os.Lstat(loop.cfg.ControlPath); err == nil {
		t.Error("the control socket was left behind after Run returned")
	}
}

// TestARefusedControlSocketIsAWarningNotAFailure: a --control naming a
// regular file cannot be listened on (and must not be deleted), and a daemon
// that cannot be asked to apply can still watch: Run says so loudly and
// carries on, and cancelling it returns nil.
func TestARefusedControlSocketIsAWarningNotAFailure(t *testing.T) {
	dir := shortDir(t)
	path := dir + "/not-a-socket"
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer
	loop, err := New(Config{
		StatePath:   filepath.Join(t.TempDir(), "state.json"),
		Interval:    time.Hour,
		ControlPath: path,
		Discover:    func(context.Context) ([]phpfpm.Target, error) { return nil, nil },
	}, slog.New(slog.NewTextHandler(&logged, nil)))
	if err != nil {
		t.Fatal(err)
	}
	stop := runLoop(t, loop)

	// The first round runs immediately; the warning is logged before it.
	time.Sleep(200 * time.Millisecond)
	stop()

	if !strings.Contains(logged.String(), "No control socket") || !strings.Contains(logged.String(), "not a socket") {
		t.Errorf("the refused control socket was not warned about; log:\n%s", logged.String())
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "keep me" {
		t.Errorf("the file at the control path was touched: %q, %v", b, err)
	}
}

// TestTheApplyHandlerAnswersWhatItCannotDo: the control socket's handler is
// POST only, says 503 when the loop is not there to take the request in
// time, and 504 when the loop took it but the round did not finish in time.
func TestTheApplyHandlerAnswersWhatItCannotDo(t *testing.T) {
	l := &Loop{applyNow: make(chan applyRequest), log: discardLogger()}

	rec := httptest.NewRecorder()
	l.handleApplyNow(rec, httptest.NewRequest(http.MethodGet, "/apply", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
		t.Errorf("GET gave %d with Allow %q, want 405 and POST", rec.Code, rec.Header().Get("Allow"))
	}

	// Nobody receiving on applyNow and a request whose context is already
	// gone: the handler must not block on the send.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	rec = httptest.NewRecorder()
	l.handleApplyNow(rec, httptest.NewRequest(http.MethodPost, "/apply", nil).WithContext(cancelled))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a loop that did not take the request gave %d, want 503", rec.Code)
	}

	// The loop takes the request and never answers; the client gives up.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-l.applyNow
		cancel()
	}()
	rec = httptest.NewRecorder()
	l.handleApplyNow(rec, httptest.NewRequest(http.MethodPost, "/apply", nil).WithContext(ctx))
	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("a round that did not finish gave %d, want 504", rec.Code)
	}
}

// serveOnSocket runs handler on an HTTP server listening on a unix socket at
// path, closed when the test ends.
func serveOnSocket(t *testing.T, path string, handler http.Handler) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(handler)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
}

// TestTheApplyClientNamesTheDaemonsRefusal: a daemon that answers anything
// but 200 is an error carrying the status, and a 200 whose body is not the
// outcome is "not readable" rather than an empty outcome taken as "nothing
// changed".
func TestTheApplyClientNamesTheDaemonsRefusal(t *testing.T) {
	dir := shortDir(t)

	failing := dir + "/failing.sock"
	serveOnSocket(t, failing, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	if _, err := ApplyNow(context.Background(), failing); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("a 500 gave %v, want an error naming the status", err)
	}

	garbled := dir + "/garbled.sock"
	serveOnSocket(t, garbled, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	if _, err := ApplyNow(context.Background(), garbled); err == nil || !strings.Contains(err.Error(), "not readable") {
		t.Errorf("a garbled answer gave %v, want 'not readable'", err)
	}
}
