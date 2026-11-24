//go:build windows

package gitcheck

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// runConversion owns Git and its filter descendants before any of them can run.
func runConversion(ctx context.Context, cmd *exec.Cmd) (err error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(job)
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return err
	}
	port, err := windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 1)
	if err != nil {
		return err
	}
	portOpen := true
	defer func() {
		if portOpen {
			windows.CloseHandle(port)
		}
	}()
	association := struct {
		key  uintptr
		port windows.Handle
	}{1, port}
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectAssociateCompletionPortInformation, uintptr(unsafe.Pointer(&association)), uint32(unsafe.Sizeof(association))); err != nil {
		return err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	ready := make(chan struct{})
	cmd.Cancel = func() error { <-ready; return windows.TerminateJobObject(job, 1) }
	if err = cmd.Start(); err != nil {
		close(ready)
		return err
	}
	assigned := false
	readyClosed := false
	waited := false
	defer func() {
		if !assigned {
			_ = cmd.Process.Kill()
		} else {
			err = errors.Join(err, windows.TerminateJobObject(job, 1))
		}
		if !readyClosed {
			close(ready)
		}
		if !waited {
			err = errors.Join(err, cmd.Wait())
		}
		if assigned {
			finished := make(chan error, 1)
			go func() {
				for {
					var message uint32
					var key uintptr
					var value *windows.Overlapped
					if e := windows.GetQueuedCompletionStatus(port, &message, &key, &value, windows.INFINITE); e != nil {
						finished <- e
						return
					}
					const activeProcessZero = 4 // JOB_OBJECT_MSG_ACTIVE_PROCESS_ZERO
					if key == 1 && message == activeProcessZero {
						var accounting struct {
							user, kernel, periodUser, periodKernel int64
							faults, total, active, terminated      uint32
						}
						if e := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&accounting)), uint32(unsafe.Sizeof(accounting)), nil); e != nil {
							finished <- e
							return
						}
						// Nested jobs also send their notifications to this port.
						if accounting.active == 0 {
							finished <- nil
							return
						}
					}
				}
			}()
			drainCtx, cancel := context.WithTimeout(context.Background(), gitTimeout)
			drainErr := awaitJobDrain(drainCtx, finished)
			cancel()
			if drainErr != nil {
				windows.CloseHandle(port)
				portOpen = false
				// Closing the port releases the notification reader before returning.
				if errors.Is(drainErr, context.DeadlineExceeded) {
					<-finished
				}
				err = errors.Join(err, errJobDrain, drainErr)
			}
		}
		if ctx.Err() != nil {
			err = errors.Join(ctx.Err(), err)
		}
	}()
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(process)
	if err = windows.AssignProcessToJobObject(job, process); err != nil {
		return err
	}
	assigned = true
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	var tid uint32
	e := windows.Thread32First(snapshot, &entry)
	for ; e == nil; e = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID == uint32(cmd.Process.Pid) {
			if tid != 0 {
				return fmt.Errorf("suspended Git has multiple threads")
			}
			tid = entry.ThreadID
		}
	}
	if !errors.Is(e, windows.ERROR_NO_MORE_FILES) {
		return fmt.Errorf("enumerate suspended Git thread: %w", e)
	}
	if tid == 0 {
		return fmt.Errorf("suspended Git thread unavailable")
	}
	thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, tid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(thread)
	count, err := windows.ResumeThread(thread)
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("unexpected suspended Git thread count: %d", count)
	}
	close(ready)
	readyClosed = true
	err = cmd.Wait()
	waited = true
	return err
}

// The completion notification is not guaranteed, so failure to observe it refuses the checkout.
func awaitJobDrain(ctx context.Context, finished <-chan error) error {
	select {
	case err := <-finished:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
