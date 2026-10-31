//go:build windows

package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/achmadss/hanse/internal/runtime"
)

// powershellHandler is a small PowerShell handler. It calls one cmdlet that
// started slowly under the old environment (ConvertFrom-Json) and one that
// repeats the measurement with a timer (Start-Sleep), so the test covers the
// module autoload of Microsoft.PowerShell.Utility and a normal script step.
const powershellHandler = `$ErrorActionPreference = 'Stop'
$parsed = '{"port":8080}' | ConvertFrom-Json
Start-Sleep -Milliseconds 100
@{ contract = 'hanse.handler/v1'; status = 'ok'; result = @{ port = $parsed.port } } |
  ConvertTo-Json -Compress -Depth 4 | Write-Output
`

// powershellSpec builds one call to the PowerShell handler. The entry
// command matches the manifest form of docs/handler-contract.md section 4.1.
func powershellSpec(t *testing.T, log func(string)) Spec {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "handler.ps1"), []byte(powershellHandler), 0o600); err != nil {
		t.Fatalf("write the handler: %v", err)
	}
	spec := helperSpec(t, "powershell", log)
	spec.Entry = []string{"powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", "handler.ps1"}
	spec.Dir = dir
	// A cold PowerShell start took 16 seconds before issue #85, so the
	// deadline must not hide a slow run.
	spec.Request.Deadline = time.Now().Add(time.Minute)
	return spec
}

// TestOrdinaryPowerShellHandlerRunsAtNormalSpeed is the acceptance test of
// issue #85: the ordinary handler path, restricted token and documented
// environment included, starts PowerShell and runs a cmdlet and a timer in
// under 3 seconds. Before the fix the same script needed 18 to 27 seconds,
// because the handler environment had no PSModulePath and Windows PowerShell
// searched for its modules on the first cmdlet use.
func TestOrdinaryPowerShellHandlerRunsAtNormalSpeed(t *testing.T) {
	logs := &logCollector{}
	start := time.Now()
	resp, err := newTestRunner(t).Run(context.Background(), powershellSpec(t, logs.log))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ordinary run: %v", err)
	}
	if resp.Status != runtime.StatusOK {
		t.Fatalf("status = %q, want ok", resp.Status)
	}
	if string(resp.Result) != `{"port":8080}` {
		t.Errorf("result = %s, want {\"port\":8080}", resp.Result)
	}
	if elapsed >= 3*time.Second {
		t.Errorf("the ordinary PowerShell handler took %v, want under 3 seconds; log lines: %v", elapsed, logs.all())
	}
}
