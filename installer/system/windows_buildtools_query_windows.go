//go:build windows

package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/luisgui1757/dotfiles/installer/platform"
)

const windowsBuildToolsInspectionTimeout = 2 * time.Minute

// QueryWindowsBuildTools is the VendorQuery of the Build Tools provider. The
// inspection runs VsDevCmd and a compile-and-run probe, which can start a
// telemetry child that does not exit. The query runs in a private job that it
// stops before it returns; the job list attribute assigns the child at its
// creation (PROC_THREAD_ATTRIBUTE_JOB_LIST, Windows 10 and Server 2016), and
// only the three pipe handles pass to the child. The limit is 2 minutes: on
// hosted Windows Server 2025 one cold inspection took more than 30 seconds.
// Do not use this job for an installer or for a compiler that another program
// started.
func QueryWindowsBuildTools(ctx context.Context, program string, input []byte, arguments ...string) ([]byte, error) {
	if !filepath.IsAbs(program) || filepath.Clean(program) != program {
		return nil, errors.New("Build Tools inspection cannot use a relative executable")
	}
	ctx, cancel := context.WithTimeout(ctx, windowsBuildToolsInspectionTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	application, err := windows.UTF16PtrFromString(program)
	if err != nil {
		return nil, err
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{program}, arguments...)))
	if err != nil {
		return nil, err
	}
	var pipes [3][2]*os.File
	defer func() {
		for _, pair := range pipes {
			for _, file := range pair {
				if file != nil {
					file.Close()
				}
			}
		}
	}()
	for i := range pipes {
		pipes[i][0], pipes[i][1], err = os.Pipe()
		if err != nil {
			return nil, err
		}
	}
	childFiles := []*os.File{pipes[0][0], pipes[1][1], pipes[2][1]}
	handles := make([]windows.Handle, len(childFiles))
	for i, file := range childFiles {
		handles[i] = windows.Handle(file.Fd())
		if err := windows.SetHandleInformation(handles[i], windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			return nil, err
		}
	}
	attributes, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		return nil, err
	}
	defer attributes.Delete()
	if err := attributes.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
		return nil, err
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(job)
	if err := windowsBuildToolsSetQueryJobLimits(job); err != nil {
		return nil, err
	}
	const procThreadAttributeJobList = 0x0002000d
	if err := attributes.Update(procThreadAttributeJobList, unsafe.Pointer(&job), unsafe.Sizeof(job)); err != nil {
		return nil, err
	}
	startup := windows.StartupInfoEx{StartupInfo: windows.StartupInfo{
		Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{})), Flags: windows.STARTF_USESTDHANDLES,
		StdInput: handles[0], StdOutput: handles[1], StdErr: handles[2],
	}, ProcThreadAttributeList: attributes.List()}
	var process windows.ProcessInformation
	flags := uint32(windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW | windows.EXTENDED_STARTUPINFO_PRESENT)
	if err := windows.CreateProcess(application, commandLine, nil, nil, true, flags, nil, nil, &startup.StartupInfo, &process); err != nil {
		return nil, err
	}
	defer windows.CloseHandle(process.Process)
	defer windows.CloseHandle(process.Thread)
	for _, file := range childFiles {
		file.Close()
	}
	if _, err := windows.ResumeThread(process.Thread); err != nil {
		return nil, errors.Join(err, windowsBuildToolsStopQueryJob(job))
	}
	output, diagnostic := vendorBoundedOutput{limit: 1 << 20}, vendorBoundedOutput{}
	completed := make(chan error, 3)
	go func() {
		_, err := io.Copy(pipes[0][1], bytes.NewReader(input))
		completed <- errors.Join(err, pipes[0][1].Close())
	}()
	go func() { _, err := io.Copy(&output, pipes[1][0]); completed <- err }()
	go func() { _, err := io.Copy(&diagnostic, pipes[2][0]); completed <- err }()
	remaining := 3
	var queryErr error
	for queryErr == nil {
		select {
		case <-ctx.Done():
			queryErr = ctx.Err()
		case err := <-completed:
			remaining--
			queryErr = err
		default:
		}
		if queryErr != nil {
			break
		}
		status, err := windows.WaitForSingleObject(process.Process, 25)
		if err != nil {
			queryErr = err
			break
		}
		if status == windows.WAIT_OBJECT_0 {
			var code uint32
			queryErr = windows.GetExitCodeProcess(process.Process, &code)
			if code != 0 {
				exitCode := int(code)
				queryErr = errors.Join(queryErr, &platform.CommandError{Message: fmt.Sprintf("exit status %d", code), ExitCode: &exitCode})
			}
			break
		}
	}
	cleanupErr := windowsBuildToolsStopQueryJob(job)
	if cleanupErr != nil {
		for _, pair := range pipes {
			for _, file := range pair {
				file.Close()
			}
		}
	}
	for ; remaining > 0; remaining-- {
		queryErr = errors.Join(queryErr, <-completed)
	}
	if err := errors.Join(queryErr, cleanupErr); err != nil {
		return nil, fmt.Errorf("Build Tools inspection failed: %w%s", err, platform.Diagnostic(diagnostic.data.Bytes()))
	}
	return output.data.Bytes(), nil
}

func windowsBuildToolsStopQueryJob(job windows.Handle) (result error) {
	deadline := time.Now().Add(5 * time.Second)
	handles, capture, captureErr := windowsBuildToolsCaptureQueryProcesses(job, deadline)
	defer func() {
		for _, handle := range handles {
			result = errors.Join(result, windows.CloseHandle(handle))
		}
	}()
	return errors.Join(captureErr, windowsBuildToolsTerminateQueryProcesses(job, handles, capture, deadline))
}

type windowsBuildToolsJobAccounting struct {
	Times      [4]int64
	PageFaults uint32
	Total      uint32
	Active     uint32
	Terminated uint32
}

var (
	windowsBuildToolsGetJobInformation = windows.NewLazySystemDLL("kernel32.dll").NewProc("QueryInformationJobObject")
	windowsBuildToolsSetJobInformation = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetInformationJobObject")
)

func windowsBuildToolsQueryJobInformation(job windows.Handle, class int32, data unsafe.Pointer, size uint32) error {
	ok, _, err := windowsBuildToolsGetJobInformation.Call(uintptr(job), uintptr(class), uintptr(data), uintptr(size), 0)
	if ok == 0 {
		return err
	}
	return nil
}

func windowsBuildToolsSetQueryJobLimits(job windows.Handle) error {
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	ok, _, err := windowsBuildToolsSetJobInformation.Call(uintptr(job), uintptr(windows.JobObjectExtendedLimitInformation), uintptr(unsafe.Pointer(&limits)), unsafe.Sizeof(limits))
	if ok == 0 {
		return err
	}
	return nil
}

func windowsBuildToolsReadJobAccounting(job windows.Handle) (windowsBuildToolsJobAccounting, error) {
	var info windowsBuildToolsJobAccounting
	err := windowsBuildToolsQueryJobInformation(job, windows.JobObjectBasicAccountingInformation, unsafe.Pointer(&info), uint32(unsafe.Sizeof(info)))
	return info, err
}

type windowsBuildToolsJobMembers struct {
	Assigned uint32
	Listed   uint32
	PIDs     [256]uintptr
}

type windowsBuildToolsQueryCapture struct {
	Before, After windowsBuildToolsJobAccounting
	AfterError    error
}

func windowsBuildToolsCaptureQueryProcesses(job windows.Handle, deadline time.Time) ([]windows.Handle, windowsBuildToolsQueryCapture, error) {
	before, err := windowsBuildToolsReadJobAccounting(job)
	if err != nil {
		return nil, windowsBuildToolsQueryCapture{Before: before}, err
	}
	return windowsBuildToolsCaptureQueryProcessesAfterAccounting(job, before, deadline)
}

func windowsBuildToolsCaptureQueryProcessesAfterAccounting(job windows.Handle, before windowsBuildToolsJobAccounting, deadline time.Time) ([]windows.Handle, windowsBuildToolsQueryCapture, error) {
	capture := windowsBuildToolsQueryCapture{Before: before}
	members, err := windowsBuildToolsCompleteQueryMembers(deadline, func(members *windowsBuildToolsJobMembers) error {
		return windowsBuildToolsQueryJobInformation(job, windows.JobObjectBasicProcessIdList, unsafe.Pointer(members), uint32(unsafe.Sizeof(*members)))
	})
	if err != nil {
		return nil, capture, err
	}
	handles := make([]windows.Handle, 0, members.Listed)
	isProcessInJob := windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")
	for _, pid := range members.PIDs[:members.Listed] {
		if pid == 0 || pid > 1<<32-1 {
			return handles, capture, errors.New("invalid Build Tools inspection member PID")
		}
		handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
		if err != nil {
			return handles, capture, fmt.Errorf("open Build Tools inspection member: %w", err)
		}
		var belongs int32
		ok, _, callErr := isProcessInJob.Call(uintptr(handle), uintptr(job), uintptr(unsafe.Pointer(&belongs)))
		if ok == 0 || belongs == 0 {
			closeErr := windows.CloseHandle(handle)
			if ok == 0 {
				return handles, capture, errors.Join(fmt.Errorf("verify Build Tools inspection membership: %w", callErr), closeErr)
			}
			return handles, capture, errors.Join(errors.New("Build Tools inspection member changed before capture"), closeErr)
		}
		handles = append(handles, handle)
	}
	capture.After, capture.AfterError = windowsBuildToolsReadJobAccounting(job)
	return handles, capture, nil
}

func windowsBuildToolsCompleteQueryMembers(deadline time.Time, query func(*windowsBuildToolsJobMembers) error) (windowsBuildToolsJobMembers, error) {
	var members windowsBuildToolsJobMembers
	for attempts := 0; ; attempts++ {
		if !time.Now().Before(deadline) {
			return members, fmt.Errorf("Build Tools inspection membership capture did not complete within five seconds; attempts=%d assigned=%d listed=%d", attempts, members.Assigned, members.Listed)
		}
		members = windowsBuildToolsJobMembers{}
		if err := query(&members); err != nil {
			return members, fmt.Errorf("capture Build Tools inspection members: %w", err)
		}
		if members.Assigned > uint32(len(members.PIDs)) || members.Listed > members.Assigned {
			return members, fmt.Errorf("invalid Build Tools inspection membership counts; assigned=%d listed=%d capacity=%d", members.Assigned, members.Listed, len(members.PIDs))
		}
		if members.Listed == members.Assigned && time.Now().Before(deadline) {
			return members, nil
		}
		time.Sleep(min(time.Millisecond, max(0, time.Until(deadline))))
	}
}

func windowsBuildToolsTerminateQueryProcesses(job windows.Handle, handles []windows.Handle, capture windowsBuildToolsQueryCapture, deadline time.Time) error {
	if err := windows.TerminateJobObject(job, 1); err != nil {
		return err
	}
	for _, handle := range handles {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return errors.New("Build Tools inspection process tree did not terminate within five seconds")
		}
		milliseconds := uint32((remaining + time.Millisecond - 1) / time.Millisecond)
		status, err := windows.WaitForSingleObject(handle, milliseconds)
		if err != nil {
			return err
		}
		if status != windows.WAIT_OBJECT_0 {
			return fmt.Errorf("Build Tools inspection process completion was not signaled within five seconds (wait %d)", status)
		}
	}
	after, err := windowsBuildToolsReadJobAccounting(job)
	if err != nil {
		return err
	}
	if after.Total != capture.Before.Total || after.Active != 0 {
		return fmt.Errorf("Build Tools inspection membership changed during termination; completion is unproved; %s", windowsBuildToolsQueryMembershipDiagnostic(job, capture, after, len(handles)))
	}
	return nil
}

func windowsBuildToolsQueryMembershipDiagnostic(job windows.Handle, capture windowsBuildToolsQueryCapture, after windowsBuildToolsJobAccounting, captured int) string {
	before := capture.Before
	detail := fmt.Sprintf("baseline(total=%d active=%d terminated=%d) final(total=%d active=%d terminated=%d) captured_handles=%d", before.Total, before.Active, before.Terminated, after.Total, after.Active, after.Terminated, captured)
	if capture.AfterError != nil {
		detail += fmt.Sprintf("; after_capture unavailable: %v", capture.AfterError)
	} else {
		detail += fmt.Sprintf("; after_capture(total=%d active=%d terminated=%d)", capture.After.Total, capture.After.Active, capture.After.Terminated)
	}
	var members windowsBuildToolsJobMembers
	if err := windowsBuildToolsQueryJobInformation(job, windows.JobObjectBasicProcessIdList, unsafe.Pointer(&members), uint32(unsafe.Sizeof(members))); err != nil {
		return fmt.Sprintf("%s; current_members unavailable: %v", detail, err)
	}
	if members.Listed > uint32(len(members.PIDs)) || members.Listed != members.Assigned {
		return fmt.Sprintf("%s; current_members assigned=%d listed=%d incomplete", detail, members.Assigned, members.Listed)
	}
	return fmt.Sprintf("%s; current_members assigned=%d listed=%d pids=%v", detail, members.Assigned, members.Listed, members.PIDs[:members.Listed])
}
