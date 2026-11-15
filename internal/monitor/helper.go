package monitor

import (
	"context"
	"time"
)

// HelperPort is the port the overlay helper serves probes on, inside its container.
const HelperPort = 7079

// HelperTokenEnv names the environment variable that holds the overlay helper's bearer token.
const HelperTokenEnv = "BERTHY_HELPER_TOKEN"

// The probe kinds a HelperProbe can name.
const (
	KindHTTP = "http"
	KindTCP  = "tcp"
)

// HelperProbe is one probe the overlay helper runs from inside the shared network. It is the body of the helper's
// POST /probe request. Target is name:port, a service name on the network. Path and Expect apply to http probes only,
// and an empty Path means "/".
type HelperProbe struct {
	Kind      string `json:"kind"`
	Target    string `json:"target"`
	Path      string `json:"path,omitempty"`
	TimeoutMS int    `json:"timeout_ms"`
	Expect    []int  `json:"expect,omitempty"`
}

// ProbeHelper runs probes from inside the overlay network. A nil error means the helper answered, and ok and detail are
// the probe's result. A non-nil error means no probe ran; its message is the detail to report.
type ProbeHelper interface {
	Probe(ctx context.Context, req HelperProbe) (ok bool, detail string, err error)
}

// ServiceProbe returns the check for a probe against a service name, which helper runs on the overlay network. A probe
// that cannot run is unknown, and its error is the detail. Unknown results never alert.
func ServiceProbe(name string, interval time.Duration, failures int, helper ProbeHelper, req HelperProbe) Check {
	return NewFunc(name, interval, failures, func(ctx context.Context) Result {
		if helper == nil {
			return Result{State: Unknown, Detail: "probe helper unavailable: not configured"}
		}
		ok, detail, err := helper.Probe(ctx, req)
		switch {
		case err != nil:
			return Result{State: Unknown, Detail: err.Error()}
		case ok:
			return Result{State: OK, Detail: detail}
		default:
			return Result{State: Critical, Detail: detail}
		}
	})
}
