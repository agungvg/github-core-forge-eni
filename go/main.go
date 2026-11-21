// main.go — the boot handshake with the trial harness ("the env").
//
// The env owns port 8080 and is the only thing this process talks to
// before the suite starts driving it. The handshake has four steps:
//
//  1. Read MAKEFUN_ENV_URL. Without it there is no env to announce to,
//     so the process stops immediately with exit code 1.
//  2. Build the forge and listen on port 0, letting the kernel pick a
//     free port. Port 8080 is the env's and is never bound here.
//  3. POST {"port": <bound port>} to <env>/register, retrying until the
//     env accepts it. A 409 means a registration already won; that is
//     fine, this process just waits like any other.
//  4. Poll GET <env>/done until it answers {"done": true}, then shut
//     the server down and exit 0. The process never leaves early.
//
// Nothing else lives here: the forge itself is assembled by app.go and
// the modules behind it.

package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// envCallTimeout bounds one request to the env. A slower env is treated
// as not ready yet and the call is retried.
const envCallTimeout = 30 * time.Second

// retryDelays is the pause before each retry, in order. The first
// failures wait briefly; later ones back off to two seconds and stay
// there, so a stuck boot stays visible in the logs without hammering
// the env.
var retryDelays = []time.Duration{
	250 * time.Millisecond,
	500 * time.Millisecond,
	1 * time.Second,
	2 * time.Second,
}

// waitForRetry sleeps before attempt n (0-based), capped at the last
// entry of retryDelays.
func waitForRetry(n int) {
	if n >= len(retryDelays) {
		n = len(retryDelays) - 1
	}
	time.Sleep(retryDelays[n])
}

// unrecoverable marks an env answer that retrying will never fix — a
// bad request or a wrong method. It ends the run instead of looping
// forever; a dropped connection, a timeout or a 5xx is retried.
type unrecoverable struct{ msg string }

func (e *unrecoverable) Error() string { return e.msg }

// attempt runs fn once. It reports true when the step is finished, or
// an error describing why it should be tried again. An
// *unrecoverable error is returned as-is so the caller can stop.
func attempt(label string, fn func() (bool, error)) error {
	tries := 0
	for {
		done, err := fn()
		if err != nil {
			if fatal, ok := err.(*unrecoverable); ok {
				return fatal
			}
			fmt.Fprintf(os.Stderr, "forge: %s failed (%s), retrying\n", label, err)
		} else if done {
			return nil
		}
		waitForRetry(tries)
		tries++
	}
}

// register announces the bound port to the env. 200 is the normal
// answer and 409 means a first registration already won — both finish
// the step. 400 and 405 are contract errors and end the program; any
// other status is retried like a network failure.
func register(client *http.Client, envBase string, port int) error {
	return attempt("register", func() (bool, error) {
		payload := []byte(fmt.Sprintf(`{"port":%d}`, port))
		req, err := http.NewRequest(http.MethodPost, envBase+"/register", bytes.NewReader(payload))
		if err != nil {
			return false, err
		}
		req.Header.Set("content-type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			return false, err
		}
		defer res.Body.Close()
		switch res.StatusCode {
		case http.StatusOK, http.StatusConflict:
			return true, nil
		case http.StatusBadRequest, http.StatusMethodNotAllowed:
			text, _ := io.ReadAll(res.Body)
			return false, &unrecoverable{msg: fmt.Sprintf("register answered HTTP %d: %s", res.StatusCode, string(text))}
		default:
			return false, nil
		}
	})
}

// waitUntilDone polls the env's /done endpoint. It finishes only when
// the body parses as JSON with "done": true. A non-2xx answer, a
// malformed body or a failed request all mean "not yet".
func waitUntilDone(client *http.Client, envBase string) error {
	return attempt("done poll", func() (bool, error) {
		res, err := client.Get(envBase + "/done")
		if err != nil {
			return false, err
		}
		defer res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return false, nil
		}
		data, err := io.ReadAll(res.Body)
		if err != nil {
			return false, err
		}
		parsed, err := parseJSON(data)
		if err != nil {
			return false, err
		}
		if doc, ok := parsed.(map[string]any); ok {
			if done, ok := doc["done"].(bool); ok && done {
				return true, nil
			}
		}
		return false, nil
	})
}

func main() {
	// The env's base URL, trailing slashes stripped so the paths joined
	// onto it below stay well formed.
	envBase := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envBase == "" {
		fmt.Fprint(os.Stderr, "forge: MAKEFUN_ENV_URL is unset, so there is no env to register with\n")
		os.Exit(1)
	}

	srv := newServer()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "forge: %s\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	httpServer := &http.Server{Handler: srv}
	go httpServer.Serve(ln)
	fmt.Fprintf(os.Stdout, "forge: listening on port %d\n", port)

	client := &http.Client{Timeout: envCallTimeout}

	if err := register(client, envBase, port); err != nil {
		fmt.Fprintf(os.Stderr, "forge: %s\n", err)
		os.Exit(1)
	}
	fmt.Fprint(os.Stdout, "forge: registered with the env, waiting for done\n")

	if err := waitUntilDone(client, envBase); err != nil {
		fmt.Fprintf(os.Stderr, "forge: %s\n", err)
		os.Exit(1)
	}
	httpServer.Close()
	fmt.Fprint(os.Stdout, "forge: env said done, exiting\n")
	os.Exit(0)
}
