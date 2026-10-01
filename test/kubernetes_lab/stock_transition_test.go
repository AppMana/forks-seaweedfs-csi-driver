package kubernetes_lab

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestStockTransitionGuardsAndSyntax(t *testing.T) {
	body, err := os.ReadFile("restore-stock-winfsp.ps1")
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	mutation := strings.Index(script, "$uninstall = Start-Process")
	if mutation < 0 {
		t.Fatal("missing MSI transition")
	}
	for _, required := range []string{"S-1-5-18", "ContainerType", "Get-Process weed,seaweedfs-mount", "WinFsp AppMana LAB ONLY", "LabDriverSHA256", "073a70e00f77423e34bed98b86e600def93393ba5822204fac57a29324db9f7a"} {
		if !strings.Contains(script[:mutation], required) {
			t.Fatal("missing pre-mutation guard", required)
		}
	}
	for _, required := range []string{"/norestart", "stock-transition\\uninstall.log", "stock-transition\\install.log", stockDriverSHA256, "Get-AuthenticodeSignature", "Microsoft Windows Hardware Compatibility Publisher", "testsigning off", "nointegritychecks off", "STOCK_MSI_RESTORED_REBOOT_REQUIRED"} {
		if !strings.Contains(script, required) {
			t.Fatal("missing transition contract", required)
		}
	}
	for _, forbidden := range []string{"Remove-Item", "Set-ItemProperty", "Copy-Item", "shutdown.exe", "Invoke-WebRequest", "testsigning on"} {
		if strings.Contains(script, forbidden) {
			t.Fatal("transition bypasses MSI or reboots implicitly", forbidden)
		}
	}
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell required for syntax check")
	}
	out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", `$tokens=$null;$errors=$null;[void][Management.Automation.Language.Parser]::ParseFile('restore-stock-winfsp.ps1',[ref]$tokens,[ref]$errors);if($errors.Count){$errors;exit 1}`).CombinedOutput()
	if err != nil {
		t.Fatalf("PowerShell syntax: %v %s", err, out)
	}
}
