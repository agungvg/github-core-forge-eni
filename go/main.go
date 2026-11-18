// main.go — the handshake between this forge and the trial harness.
// Ported from main.ts.
//
// The harness (the "env") is the only client that matters at boot. It owns
// port 8080, waits for this process to announce the port it bound, probes
// GET /healthz on that port, runs the whole conformance suite and the timed
// workloads against it, and finally answers GET /done with {"done": true}.
// Everything in this file is that handshake; the forge itself is built by
// app.go and the modules behind it.
//
// The order of business:
//   1. Read MAKEFUN_ENV_URL. With no env to talk to there is nothing to do,
//      so the process exits 1 right away.
//   2. Build the forge and listen on port 0, letting the kernel pick a free
//      port. Port 8080 belongs to the env and is never bound here.
//   3. POST {"port": <the bound port>} to <env>/register, retrying until the
//      env takes it. A 409 means another entry registered first; that is not
//      an error for this process, which simply waits like any other.
//   4. Poll GET <env>/done until it answers {"done": true}, then close the
//      server and exit 0. The process never leaves early on its own say-so.

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

// One request to the env may take this long before it is given up on and
// retried. Retries start this far apart, double each time, and never wait
// longer than the cap, so a still-starting env is polled often.
const callTimeout = 30 * time.Second
const backoffFirst = 250 * time.Millisecond
const backoffLimit = 2000 * time.Millisecond

func pause(d time.Duration) { time.Sleep(d) }

// How long to wait after the nth failed attempt: exponential from
// backoffFirst, capped at backoffLimit.
func backoffAfter(failures int) time.Duration {
	if failures > 8 {
		failures = 8
	}
	grown := backoffFirst * time.Duration(1<<uint(failures))
	if grown > backoffLimit {
		return backoffLimit
	}
	return grown
}

// Thrown by a step when the env's answer means the contract itself is wrong
// (a bad request, a wrong method); unlike a dropped connection or a slow env,
// that will not heal by retrying, so it ends the process.
type fatalEnvError struct{ msg string }

func (e *fatalEnvError) Error() string { return e.msg }

// What one attempt of a retried step reports: either the step is finished,
// or it should be tried again after a backoff.
type stepOutcome int

const (
	settled stepOutcome = iota
	retry
)

// Run `step` until it reports settled, backing off between failures. A step
// that returns a fatalEnvError ends the run at once; anything else — a
// refused connection, a timeout, a 5xx — is treated as "the env is not ready
// yet".
func keepTrying(label string, step func() (stepOutcome, error)) error {
	failures := 0
	for {
		outcome, err := step()
		if err != nil {
			if fe, ok := err.(*fatalEnvError); ok {
				return fe
			}
			fmt.Fprintf(os.Stderr, "entry: %s failed (%s), retrying\n", label, err.Error())
		} else if outcome == settled {
			return nil
		}
		failures++
		pause(backoffAfter(failures))
	}
}

func main() {
	// The env's base URL, trailing slashes stripped so the paths below join up.
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

	client := &http.Client{Timeout: callTimeout}

	// Announce the bound port to the env. 200 is the normal answer and 409
	// means a first registration already won; 400 and 405 are contract errors
	// and end the program, while any other status is retried like a network
	// failure.
	announcePort := func() (stepOutcome, error) {
		body := []byte(fmt.Sprintf(`{"port":%d}`, port))
		req, err := http.NewRequest("POST", envBase+"/register", bytes.NewReader(body))
		if err != nil {
			return retry, err
		}
		req.Header.Set("content-type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			return retry, err
		}
		defer res.Body.Close()
		if res.StatusCode == 200 || res.StatusCode == 409 {
			return settled, nil
		}
		if res.StatusCode == 400 || res.StatusCode == 405 {
			text, _ := io.ReadAll(res.Body)
			return retry, &fatalEnvError{msg: fmt.Sprintf("register answered HTTP %d: %s", res.StatusCode, string(text))}
		}
		return retry, nil
	}

	// One poll of GET /done. Settled only when the body says the trial is
	// over; a non-OK answer, a malformed body, or a failed request all mean
	// "not yet".
	pollDone := func() (stepOutcome, error) {
		res, err := client.Get(envBase + "/done")
		if err != nil {
			return retry, err
		}
		defer res.Body.Close()
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			data, err := io.ReadAll(res.Body)
			if err != nil {
				return retry, err
			}
			v, err := parseJSON(data)
			if err != nil {
				return retry, err
			}
			if m, ok := v.(map[string]any); ok {
				if done, ok := m["done"].(bool); ok && done {
					return settled, nil
				}
			}
		}
		return retry, nil
	}

	if err := keepTrying("register", announcePort); err != nil {
		fmt.Fprintf(os.Stderr, "entry: %s\n", err.Error())
		os.Exit(1)
	}
	fmt.Fprint(os.Stdout, "entry: registered with the env, waiting for done\n")

	if err := keepTrying("done poll", pollDone); err != nil {
		fmt.Fprintf(os.Stderr, "entry: %s\n", err.Error())
		os.Exit(1)
	}
	httpServer.Close()
	fmt.Fprint(os.Stdout, "entry: env said done, exiting\n")
	os.Exit(0)
}
