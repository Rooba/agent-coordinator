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
	"time"
	"unicode/utf16"
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

var errTaskNotRunning = errors.New("scheduled host broker is not running")

const schedulerCleanupTimeout = 5 * time.Second

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
			_, err = file.Write(utf16LE(xmlBody))
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
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), schedulerCleanupTimeout)
			cleanupErr := run(cleanupCtx, tool, "/Delete", "/TN", name, "/F")
			cancel()
			return SchedulePlan{}, errors.Join(err, cleanupErr)
		}
	case ScheduleUninstall:
		plan.Steps = [][]string{{"/End", "/TN", name}, {"/Delete", "/TN", name, "/F"}}
		if dryRun {
			return plan, nil
		}
		if err := run(ctx, tool, plan.Steps[0]...); err != nil && !errors.Is(err, errTaskNotRunning) {
			return SchedulePlan{}, err
		}
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

// ScheduleTaskName is the Task Scheduler entry for one Windows user's broker,
// so anything that needs to find that task derives the name the same way.
func ScheduleTaskName(sid string) string {
	hash := sha256.Sum256([]byte(sid))
	return "Agent Coordinator Host Broker-" + hex.EncodeToString(hash[:6])
}

func scheduleName(sid string) string {
	return `\` + ScheduleTaskName(sid)
}

func scheduleXML(executable, sid string) (string, error) {
	if strings.TrimSpace(sid) == "" || strings.ContainsRune(sid, 0) {
		return "", errors.New("invalid current-user SID")
	}
	command, workingDir := xmlEscape(executable), xmlEscape(filepath.Dir(executable))
	user := xmlEscape(sid)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>Agent Coordinator Windows host broker</Description></RegistrationInfo>
  <Triggers><LogonTrigger><Enabled>true</Enabled><UserId>%s</UserId></LogonTrigger></Triggers>
  <Principals><Principal id="CurrentUser"><UserId>%s</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Settings><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy><DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries><StopIfGoingOnBatteries>false</StopIfGoingOnBatteries><AllowHardTerminate>true</AllowHardTerminate><StartWhenAvailable>true</StartWhenAvailable><AllowStartOnDemand>true</AllowStartOnDemand><Enabled>true</Enabled><ExecutionTimeLimit>PT0S</ExecutionTimeLimit></Settings>
  <Actions Context="CurrentUser"><Exec><Command>%s</Command><Arguments>host run</Arguments><WorkingDirectory>%s</WorkingDirectory></Exec></Actions>
</Task>`, user, user, command, workingDir), nil
}

// utf16LE encodes the task XML the way schtasks /XML expects: a UTF-16 LE
// byte order mark followed by little-endian code units.
func utf16LE(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 0, 2+2*len(units))
	out = append(out, 0xFF, 0xFE)
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

func xmlEscape(value string) string {
	var result strings.Builder
	_ = xml.EscapeText(&result, []byte(value))
	return result.String()
}
