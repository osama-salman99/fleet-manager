package deployer

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestVerifyNoticesAHealthyServiceWithinHalfASecond(t *testing.T) {
	old, oldSettle := unitActive, unitSettle
	unitActive = func(string) string { return "active" }
	unitSettle = time.Second
	defer func() { unitActive, unitSettle = old, oldSettle }()

	// The app answers its health URL 1 s after the restart, like a fast JVM.
	up := time.Now().Add(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if time.Now().Before(up) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	start := time.Now()
	if err := verify(Service{Name: "app", Unit: "app.service", HealthURL: srv.URL}, "v1-x"); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 1700*time.Millisecond {
		t.Fatalf("healthy after 1 s but the deploy only noticed after %s", took)
	}

	// No health URL: systemd's "active" alone is not believed for the first 3 s.
	start = time.Now()
	if err := verify(Service{Name: "bare", Unit: "bare.service"}, "v1-x"); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < unitSettle {
		t.Fatalf("a service with no health check counted as started after only %s", took)
	}
}

// Every way verify can end for a service with a unit.
func TestVerifyPaths(t *testing.T) {
	// Faster clocks than production so the failure cases (which wait out the whole
	// health wait) keep the suite quick; the production poll is pinned by the test above.
	prevActive, prevSettle, prevPoll := unitActive, unitSettle, healthPollEvery
	unitSettle, healthPollEvery = 400*time.Millisecond, 100*time.Millisecond
	defer func() { unitActive, unitSettle, healthPollEvery = prevActive, prevSettle, prevPoll }()

	status := func(code int) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	// The clock starts when the case runs, not when the table is built.
	activeAfter := func(d time.Duration) func() func(string) string {
		return func() func(string) string {
			at := time.Now().Add(d)
			return func(string) string {
				if time.Now().Before(at) {
					return "activating"
				}
				return "active"
			}
		}
	}
	always := func(st string) func() func(string) string {
		return func() func(string) string { return func(string) string { return st } }
	}
	cases := []struct {
		name    string
		active  func() func(string) string
		health  string
		ok      bool
		atLeast time.Duration
	}{
		{"active and healthy", activeAfter(0), status(200), true, 0},
		{"health answers 4xx: the app is up", activeAfter(0), status(404), true, 0},
		{"unit active only after a while", activeAfter(600 * time.Millisecond), status(200), true, 500 * time.Millisecond},
		{"unit never active", always("failed"), status(200), false, time.Second},
		{"systemd cannot be asked", always(""), status(200), false, time.Second},
		{"health always 5xx", activeAfter(0), status(503), false, time.Second},
		{"health not listening", activeAfter(0), "http://127.0.0.1:1/", false, time.Second},
		{"no health URL: active long enough", activeAfter(0), "", true, 400 * time.Millisecond},
		{"no health URL: never stays active", always("activating"), "", false, time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			unitActive = c.active()
			start := time.Now()
			err := verify(Service{Name: "app", Unit: "app.service", HealthURL: c.health, HealthWaitSec: 1}, "v1-x")
			took := time.Since(start)
			if (err == nil) != c.ok {
				t.Fatalf("want ok=%v, got %v", c.ok, err)
			}
			if took < c.atLeast {
				t.Fatalf("finished after %s, before %s", took, c.atLeast)
			}
			if c.ok && took > c.atLeast+700*time.Millisecond {
				t.Fatalf("noticed the service %s late (after %s)", took-c.atLeast, took)
			}
		})
	}
}

func TestVerifySkipsWhatItCannotCheck(t *testing.T) {
	prev := unitActive
	unitActive = func(string) string { t.Fatal("asked systemd about a service with nothing to check"); return "" }
	defer func() { unitActive = prev }()
	if err := verify(Service{Name: "nothing"}, "v1-x"); err != nil {
		t.Fatal(err)
	}
}
