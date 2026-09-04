package hostbroker

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestSchedulerDryRunUsesInteractiveLeastPrivilegeXML(t *testing.T) {
	called := false
	plan, err := applySchedule(context.Background(), ScheduleInstall, "/Program Files/ac<&>.exe", "S-1-5-21-100", "/Windows/System32/schtasks.exe", true,
		func(context.Context, string, ...string) error { called = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if called || len(plan.Steps) != 2 || plan.Steps[0][0] != "/Create" || plan.Steps[1][0] != "/Run" {
		t.Fatalf("dry-run plan = %+v, called=%v", plan, called)
	}
	for _, required := range []string{"<LogonType>InteractiveToken</LogonType>", "<RunLevel>LeastPrivilege</RunLevel>", "<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>", "<Arguments>host run</Arguments>", "/Program Files/ac&lt;&amp;&gt;.exe"} {
		if !strings.Contains(plan.XML, required) {
			t.Fatalf("scheduler XML missing %q", required)
		}
	}
	for _, forbidden := range []string{"/RU", "/RP", testToken, "SessionSecret"} {
		if strings.Contains(plan.XML+strings.Join(plan.Steps[0], " "), forbidden) {
			t.Fatalf("scheduler plan contains %q", forbidden)
		}
	}
}

func TestScheduleUninstallDoesNotRequireExistingExecutable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "moved.exe")
	if err := validateScheduleExecutable(ScheduleUninstall, missing); err != nil {
		t.Fatalf("uninstall rejected moved executable: %v", err)
	}
	if err := validateScheduleExecutable(ScheduleInstall, missing); err == nil {
		t.Fatal("install accepted missing executable")
	}
}

func TestSchedulerUninstallEndsThenDeletes(t *testing.T) {
	var calls [][]string
	_, err := applySchedule(context.Background(), ScheduleUninstall, "/opt/ac.exe", "S-1-5-21-100", "/Windows/System32/schtasks.exe", false,
		func(_ context.Context, _ string, args ...string) error {
			calls = append(calls, append([]string(nil), args...))
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0][0] != "/End" || calls[1][0] != "/Delete" {
		t.Fatalf("uninstall calls = %v", calls)
	}
	if scheduleName("S-1") == scheduleName("S-2") {
		t.Fatal("task names collide across users")
	}
}
