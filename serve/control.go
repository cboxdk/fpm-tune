package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// The control socket: how an operator asks a watching daemon to act once.
//
// A daemon in advisory mode holds the state and the plan, and holds the
// state lock for the life of the process — so `fpm-tune apply` run beside it
// is refused, correctly: two writers of one state file is how an hour of
// learning gets discarded. That left no way to apply what the daemon showed
// without switching it to apply mode wholesale. This is that way: a unix
// socket, root-only by its file mode, on which "apply once" runs one round
// with applying forced on and the hysteresis waived (the operator has seen
// the plan and asked for it), and answers with what changed. The daemon stays
// advisory afterwards. fpm-tune apply-now is the client; fpm-tune top's a key
// runs it.

// ApplyOutcome is what one forced round did.
type ApplyOutcome struct {
	// Changed lists the pools resized, with the reason the plan gave.
	Changed []HistoryEvent `json:"changed"`

	// Message says why nothing changed, when nothing did: every pool was at
	// its plan already, or the host could not be written.
	Message string `json:"message,omitempty"`

	// Error is the apply's failure, in its own words, when it failed.
	Error string `json:"error,omitempty"`
}

// applyRequest is one "apply once", with the channel its outcome goes back
// on.
type applyRequest struct {
	reply chan ApplyOutcome
}

// controlPathFor is the socket beside the state file: the directory is
// root's already, and the socket inherits that.
func controlPathFor(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), "control.sock")
}

// startControl listens on the control socket. A stale socket file from a
// daemon that died is removed first, and only a socket is: a --control that
// names some other file by mistake must not delete it. The new socket is
// root-only from the moment it exists, because whoever can write to it can
// reconfigure php-fpm: it is created under a umask that leaves no permission
// to anyone else, and then narrowed to the owner outright.
func (l *Loop) startControl() (*http.Server, error) {
	path := l.cfg.ControlPath
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("cannot create the control socket's directory: %w", err)
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket; refusing to replace it", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("cannot replace a stale control socket at %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("cannot inspect the control socket path %s: %w", path, err)
	}
	// Process-wide, so restored at once; startup is the one goroutine that
	// creates files at this point.
	old := syscall.Umask(0o077)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, fmt.Errorf("cannot listen on the control socket %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()

		return nil, fmt.Errorf("cannot restrict the control socket %s: %w", path, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/apply", l.handleApplyNow)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			l.log.Error("Control socket stopped", "path", path, "error", err)
		}
	}()

	return srv, nil
}

// handleApplyNow answers POST /apply: hands the request to the loop, which
// runs one forced round, and returns what it did. The wait is bounded by the
// request's context and by a round's worst case.
func (l *Loop) handleApplyNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST only", http.StatusMethodNotAllowed)

		return
	}
	req := applyRequest{reply: make(chan ApplyOutcome, 1)}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	select {
	case l.applyNow <- req:
	case <-ctx.Done():
		http.Error(w, "the daemon is busy; try again", http.StatusServiceUnavailable)

		return
	}
	select {
	case outcome := <-req.reply:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(outcome)
	case <-ctx.Done():
		http.Error(w, "the round did not finish in time", http.StatusGatewayTimeout)
	}
}

// ApplyNow is the client side: asks the daemon behind the control socket to
// apply once and returns what it did. Reaching the socket needs the file
// mode's permission, which is root's.
func ApplyNow(ctx context.Context, controlPath string) (ApplyOutcome, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer

				return d.DialContext(ctx, "unix", controlPath)
			},
		},
		Timeout: 3 * time.Minute,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://fpm-tune/apply", nil)
	if err != nil {
		return ApplyOutcome{}, err
	}
	res, err := client.Do(req)
	if err != nil {
		return ApplyOutcome{}, fmt.Errorf("cannot reach the daemon at %s (is fpm-tune serve running, and are you root?): %w", controlPath, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return ApplyOutcome{}, fmt.Errorf("the daemon answered %s", res.Status)
	}
	var out ApplyOutcome
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return ApplyOutcome{}, fmt.Errorf("the daemon's answer was not readable: %w", err)
	}

	return out, nil
}
