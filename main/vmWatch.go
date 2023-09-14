package main

import (
	"bytes"
	"fmt"
	"github.com/Azure/azure-docker-extension/pkg/vmextension"
	"github.com/go-kit/kit/log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type VMWatchStatus string

const (
	Disabled VMWatchStatus = "Disabled"
	Running  VMWatchStatus = "Running"
	Failed   VMWatchStatus = "Failed"
)

func (p VMWatchStatus) GetStatusType() StatusType {
	switch p {
	case Disabled:
		return StatusWarning
	case Failed:
		return StatusError
	default:
		return StatusSuccess
	}
}

type VMWatchResult struct {
	Status VMWatchStatus
	Error  error
}

func (r *VMWatchResult) GetMessage() string {
	switch r.Status {
	case Disabled:
		return "VMWatch is disabled"
	case Failed:
		return fmt.Sprintf("VMWatch failed: %v", r.Error)
	default:
		return "VMWatch is running"
	}
}

// We will setup and execute VMWatch as a separate process. Ideally VMWatch should run indefinitely,
// but as a best effort we will attempt at most 3 times to run the process
func executeVMWatch(ctx *log.Context, s *vmWatchSettings, h vmextension.HandlerEnvironment, vmWatchResultChannel chan VMWatchResult) {
	pid := -1
	combinedOutput := &bytes.Buffer{}
	var vmWatchErr error

	defer func() {
		ctx.Log("error", fmt.Sprintf("Signaling VMWatchStatus is Failed due to reaching max of %d retries", VMWatchMaxProcessAttempts))
		vmWatchResultChannel <- VMWatchResult{Status: Failed, Error: vmWatchErr}
	}()

	// Best effort to start VMWatch process each time it fails
	for i := 1; i <= VMWatchMaxProcessAttempts; i++ {
		// Setup command
		cmd, err := setupVMWatchCommand(s, h)
		if err != nil {
			vmWatchErr = fmt.Errorf("[%v][PID %d] Err: %w", time.Now().UTC().Format(time.RFC3339), pid, err)
			ctx.Log("error", fmt.Sprintf("Attempt %d: VMWatch setup failed: %s", i, vmWatchErr.Error()))
			continue
		}

		ctx.Log("event", fmt.Sprintf("Attempt %d: Setup VMWatch command: %s\nArgs: %v\nDir: %s\nEnv: %v\n", i, cmd.Path, cmd.Args, cmd.Dir, cmd.Env))

		// TODO: Combined output may get excessively long, especially since VMWatch is a long running process
		// We should trim the output or get from Stderr
		combinedOutput.Reset()
		cmd.Stdout = combinedOutput
		cmd.Stderr = combinedOutput

		// Start command
		err = cmd.Start()
		if cmd.Process == nil {
			pid = -1
		} else {
			pid = cmd.Process.Pid
		}
		if err != nil {
			vmWatchErr = fmt.Errorf("[%v][PID %d] Err: %w\nOutput: %s", time.Now().UTC().Format(time.RFC3339), pid, err, combinedOutput.String())
			ctx.Log("error", fmt.Sprintf("Attempt %d: VMWatch failed to start: %s", i, vmWatchErr.Error()))
			continue
		}
		ctx.Log("event", fmt.Sprintf("Attempt %d: VMWatch process started with pid %d", i, pid))

		// VMWatch should run indefinitely, if process exists we expect an error
		err = cmd.Wait()
		vmWatchErr = fmt.Errorf("[%v][PID %d] Err: %w\nOutput: %s", time.Now().UTC().Format(time.RFC3339), pid, err, combinedOutput.String())
		ctx.Log("error", fmt.Sprintf("Attempt %d: VMWatch process exited: %s", i, vmWatchErr.Error()))
	}
}

func killVMWatch(ctx *log.Context, cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		ctx.Log("event", fmt.Sprintf("VMWatch is not running, not killing process."))
		return nil
	}

	if err := cmd.Process.Kill(); err != nil {
		ctx.Log("error", fmt.Sprintf("Failed to kill VMWatch process with PID %d. Error: %v", cmd.Process.Pid, err))
		return err
	}

	ctx.Log("event", fmt.Sprintf("Successfully killed VMWatch process with PID %d", cmd.Process.Pid))
	return nil
}

func setupVMWatchCommand(s *vmWatchSettings, h vmextension.HandlerEnvironment) (*exec.Cmd, error) {
	processDirectory, err := GetProcessDirectory()
	if err != nil {
		return nil, err
	}

	args := []string{"--config", GetVMWatchConfigFullPath(processDirectory)}
	
	args = append(args, "--input-filter")
	if s.Tests != nil && len(s.Tests) > 0 {
		args = append(args, strings.Join(s.Tests, ":"))
	} else {
		args = append(args, VMWatchDefaultTests)
	}

	cmd := exec.Command(GetVMWatchBinaryFullPath(processDirectory), args...)

	cmd.Env = GetVMWatchEnvironmentVariables(s.ParameterOverrides, h)

	return cmd, nil
}

func GetProcessDirectory() (string, error) {
	p, err := filepath.Abs(os.Args[0])
	if err != nil {
		return "", err
	}

	return filepath.Dir(p), nil
}

func GetVMWatchConfigFullPath(processDirectory string) string {
	return filepath.Join(processDirectory, "VMWatch", VMWatchConfigFileName)
}

func GetVMWatchBinaryFullPath(processDirectory string) string {
	binaryName := VMWatchBinaryNameAmd64
	if strings.Contains(os.Args[0], AppHealthBinaryNameArm64) {
		binaryName = VMWatchBinaryNameArm64
	}

	return filepath.Join(processDirectory, "VMWatch", binaryName)
}

func GetVMWatchEnvironmentVariables(parameterOverrides map[string]interface{}, h vmextension.HandlerEnvironment) []string {
	var arr []string
	for key, value := range parameterOverrides {
		arr = append(arr, fmt.Sprintf("%s=%s", key, value))
	}

	arr = append(arr, fmt.Sprintf("SIGNAL_FOLDER=%s", HandlerEnvironmentEventsFolderPath))
	arr = append(arr, fmt.Sprintf("VERBOSE_LOG_FILE_FULL_PATH=%s", filepath.Join(h.HandlerEnvironment.LogFolder, VMWatchVerboseLogFileName)))

	return arr
}
