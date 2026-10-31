package runner

import (
	"strings"
	"testing"

	"github.com/achmadss/hanse/internal/runtime"
)

// TestBuildEnvPrivilegedFlag covers HANSE_PRIVILEGED: 1 for a privileged
// handler and 0 otherwise (docs/handler-contract.md section 4.2).
func TestBuildEnvPrivilegedFlag(t *testing.T) {
	req := &runtime.Request{ID: "op_test", Contract: runtime.ContractV1}
	cases := []struct {
		privileged bool
		want       string
	}{
		{true, "HANSE_PRIVILEGED=1"},
		{false, "HANSE_PRIVILEGED=0"},
	}
	for _, c := range cases {
		env := buildEnv(req, t.TempDir(), t.TempDir(), nil, c.privileged)
		found := false
		for _, item := range env {
			if item == c.want {
				found = true
			}
			if strings.HasPrefix(item, "HANSE_PRIVILEGED=") && item != c.want {
				t.Errorf("privileged=%v: env holds %q, want %q", c.privileged, item, c.want)
			}
		}
		if !found {
			t.Errorf("privileged=%v: env %v has no %s", c.privileged, env, c.want)
		}
	}
}
