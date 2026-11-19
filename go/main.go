// main.go — the handshake between this forge and the trial harness.
//
// The harness (the "env") is the only client that matters at boot. It
// owns port 8080, waits for this process to announce the port it bound,
// probes GET /healthz on that port, runs the conformance suite and the
// timed workloads against it, and finally answers GET /done with
// {"done": true}. Everything in this file is that handshake; the forge
// itself is built by app.go and the modules behind it.
//
// The sequence:
//  1. Read MAKEFUN_ENV_URL. With no env to talk to there is nothing to
//     do, so the process exits 1 right away.
//  2. Build the forge and listen on port 0, letting the kernel pick a
//     free port. Port 8080 belongs to the env and is never bound here.
//  3. POST {"port": <the bound port>} to <env>/register, retrying until
//     the env takes it. A 409 means another entry registered first; that
//     is not an error for this process, which simply waits like any
//     other.
//  4. Poll GET <env>/done until it answers {"done": true}, then shut
//     the server down and exit 0. The process never leaves early on its
//     own say-so.

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

// callTimeout bounds any single request to the env; a slower env is
// treated as not-ready-yet and retried.
const callTimeout = 30 * time.Second

// backoffFirst is the wait after the first failed attempt; backoffLimit
// is the longest wait between attempts.
const backoffFirst = 250 * time.Millisecond
const backoffLimit = 2000 * time.Millisecond

// pause sleeps for d. It exists so the retry loop reads as intent rather
// than a bare time.Sleep call.
func pause(d time.Duration) { time.Sleep(d) }

// backoffAfter returns the wait following the nth failure: exponential
// growth from backoffFirst, capped at backoffLimit. The shift count is
// clamped so the multiplication can never overflow.
func backoffAfter(failures int) time.Duration {
	if failures > 8 {
		failures = 8
	}
	wait := backoffFirst * time.Duration(1<<uint(failures))
	if wait > backoffLimit {
		return backoffLimit
	}
	return wait
}

// fatalEnvError is thrown by a step when the env's answer means the
// contract itself is wrong — a bad request, a wrong method. Unlike a
// dropped connection or a slow env, that will not heal by retrying, so
// it ends the process instead of looping forever.
type fatalEnvError struct{ msg string }

func (e *fatalEnvError) Error() string { return e.msg }

// stepOutcome is what one attempt of a retried step reports: either the
// step is finished, or it should be tried again after a backoff.
type stepOutcome int

const (
	settled stepOutcome = iota // the step is done; stop retrying
	retry                      // try the step again after a backoff
)

// keepTrying runs step until it reports settled, backing off between
// failures. A fatalEnvError ends the run at once; anything else — a
// refused connection, a timeout, a 5xx — is treated as "the env is not
// ready yet" and retried, with a log line per failure so a stuck boot
// is visible in the process output.
func keepTrying(label string, step func() (stepOutcome, error)) error {
	failures := 0
	for {
		outcome, err := step()
		if err != nil {
			if fatal, ok := err.(*fatalEnvError); ok {
				return fatal
			}
			fmt.Fprintf(os.Stderr, "entry: %s failed (%s), retrying\n", label, err.Error())
		} else if outcome == settled {
			return nil
		}
		failures++
		pause(backoffAfter(failures))
	}
}

// envClient builds the HTTP client used for every call to the env.
func envClient() *http.Client {
	return &http.Client{Timeout: callTimeout}
}

// registerStep returns a step that announces port to the env's /register
// endpoint. 200 is the normal answer and 409 means a first registration
// already won — both settle the step. 400 and 405 are contract errors
// and end the program; any other status is retried like a network
// failure.
func registerStep(client *http.Client, envBase string, port int) func() (stepOutcome, error) {
	return func() (stepOutcome, error) {
		payload := []byte(fmt.Sprintf(`{"port":%d}`, port))
		req, err := http.NewRequest("POST", envBase+"/register", bytes.NewReader(payload))
		if err != nil {
			return retry, err
		}
		req.Header.Set("content-type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			return retry, err
		}
		defer res.Body.Close()
		switch res.StatusCode {
		case 200, 409:
			return settled, nil
		case 400, 405:
			text, _ := io.ReadAll(res.Body)
			return retry, &fatalEnvError{msg: fmt.Sprintf("register answered HTTP %d: %s", res.StatusCode, string(text))}
		default:
			return retry, nil
		}
	}
}

// doneStep returns a step that polls the env's /done endpoint. It
// settles only when the body parses as JSON with "done": true; a
// non-OK answer, a malformed body, or a failed request all mean "not
// yet".
func doneStep(client *http.Client, envBase string) func() (stepOutcome, error) {
	return func() (stepOutcome, error) {
		res, err := client.Get(envBase + "/done")
		if err != nil {
			return retry, err
		}
		defer res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return retry, nil
		}
		data, err := io.ReadAll(res.Body)
		if err != nil {
			return retry, err
		}
		parsed, err := parseJSON(data)
		if err != nil {
			return retry, err
		}
		if doc, ok := parsed.(map[string]any); ok {
			if done, ok := doc["done"].(bool); ok && done {
				return settled, nil
			}
		}
		return retry, nil
	}
}

func main() {
	// The env's base URL, trailing slashes stripped so the paths joined
	// onto it below are well formed.
	envBase := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envBase == "" {
		fmt.Fprint(os.Stderr, "entry: MAKEFUN_ENV_URL is unset, so there is no env to register with\n")
		os.Exit(1)
	}

	srv := newServer()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "entry: %s\n", err.Error())
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	httpServer := &http.Server{Handler: srv}
	go httpServer.Serve(ln)
	fmt.Fprintf(os.Stdout, "entry: forge listening on port %d\n", port)

	client := envClient()

	if err := keepTrying("register", registerStep(client, envBase, port)); err != nil {
		fmt.Fprintf(os.Stderr, "entry: %s\n", err.Error())
		os.Exit(1)
	}
	fmt.Fprint(os.Stdout, "entry: registered with the env, waiting for done\n")

	if err := keepTrying("done poll", doneStep(client, envBase)); err != nil {
		fmt.Fprintf(os.Stderr, "entry: %s\n", err.Error())
		os.Exit(1)
	}
	httpServer.Close()
	fmt.Fprint(os.Stdout, "entry: env said done, exiting\n")
	os.Exit(0)
}
