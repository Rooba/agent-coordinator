package hostbroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type ScheduleAction string

const (
	ScheduleInstall   ScheduleAction = "install"
	ScheduleUninstall ScheduleAction = "uninstall"
)

type SchedulePlan struct {
	TaskName string
	Tool     string
	Steps    [][]string
	XML      string
}

type commandRunner func(context.Context, string, ...string) error

func applySchedule(ctx context.Context, action ScheduleAction, executable, sid, tool string, dryRun bool, run commandRunner) (SchedulePlan, error) {
	if !filepath.IsAbs(executable) || strings.ContainsRune(executable, 0) {
		return SchedulePlan{}, errors.New("host executable must be an absolute path")
	}
	if !filepath.IsAbs(tool) || sid == "" || run == nil {
		return SchedulePlan{}, errors.New("invalid Task Scheduler configuration")
	}
	name := scheduleName(sid)
	plan := SchedulePlan{TaskName: name, Tool: tool}
	switch action {
	case ScheduleInstall:
		xmlBody, err := scheduleXML(executable, sid)
		if err != nil {
			return SchedulePlan{}, err
		}
		plan.XML = xmlBody
		plan.Steps = [][]string{{"/Create", "/TN", name, "/XML", "<temporary-xml>", "/F"}, {"/Run", "/TN", name}}
		if dryRun {
			return plan, nil
		}
		file, err := os.CreateTemp("", "agent-coordinator-task-*.xml")
		if err != nil {
			return SchedulePlan{}, err
		}
		path := file.Name()
		defer os.Remove(path)
		if err = file.Chmod(0o600); err == nil {
			_, err = file.WriteString(xmlBody)
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return SchedulePlan{}, err
		}
		create := []string{"/Create", "/TN", name, "/XML", path, "/F"}
		if err := run(ctx, tool, create...); err != nil {
			return SchedulePlan{}, err
		}
		if err := run(ctx, tool, "/Run", "/TN", name); err != nil {
			_ = run(context.Background(), tool, "/Delete", "/TN", name, "/F")
			return SchedulePlan{}, err
		}
	case ScheduleUninstall:
		plan.Steps = [][]string{{"/End", "/TN", name}, {"/Delete", "/TN", name, "/F"}}
		if dryRun {
			return plan, nil
		}
		_ = run(ctx, tool, plan.Steps[0]...)
		if err := run(ctx, tool, plan.Steps[1]...); err != nil {
			return SchedulePlan{}, err
		}
	default:
		return SchedulePlan{}, errors.New("unknown Task Scheduler action")
	}
	return plan, nil
}

func validateScheduleExecutable(action ScheduleAction, executable string) error {
	if action != ScheduleInstall {
		return nil
	}
	info, err := os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("host executable must be an existing regular file")
	}
	return nil
}

func scheduleName(sid string) string {
	hash := sha256.Sum256([]byte(sid))
	return `\Agent Coordinator Host Broker-` + hex.EncodeToString(hash[:6])
}

func scheduleXML(executable, sid string) (string, error) {
	if strings.TrimSpace(sid) == "" || strings.ContainsRune(sid, 0) {
		return "", errors.New("invalid current-user SID")
	}
	command, workingDir := xmlEscape(executable), xmlEscape(filepath.Dir(executable))
	user := xmlEscape(sid)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>Agent Coordinator Windows host broker</Description></RegistrationInfo>
  <Triggers><LogonTrigger><Enabled>true</Enabled><UserId>%s</UserId></LogonTrigger></Triggers>
  <Principals><Principal id="CurrentUser"><UserId>%s</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Settings><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy><DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries><StopIfGoingOnBatteries>false</StopIfGoingOnBatteries><AllowHardTerminate>true</AllowHardTerminate><StartWhenAvailable>true</StartWhenAvailable><AllowStartOnDemand>true</AllowStartOnDemand><Enabled>true</Enabled><ExecutionTimeLimit>PT0S</ExecutionTimeLimit></Settings>
  <Actions Context="CurrentUser"><Exec><Command>%s</Command><Arguments>host run</Arguments><WorkingDirectory>%s</WorkingDirectory></Exec></Actions>
</Task>`, user, user, command, workingDir), nil
}

func xmlEscape(value string) string {
	var result strings.Builder
	_ = xml.EscapeText(&result, []byte(value))
	return result.String()
}
