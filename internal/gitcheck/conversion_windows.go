//go:build windows

package gitcheck

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// runConversion owns Git and its filter descendants before any of them can run.
func runConversion(ctx context.Context, cmd *exec.Cmd) (err error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	jobClosed := false
	defer func() {
		if !jobClosed {
			windows.CloseHandle(job)
		}
	}()
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	var pin runtime.Pinner
	pin.Pin(&limits)
	_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)))
	pin.Unpin()
	if err != nil {
		return err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	ready := make(chan struct{})
	var jobMu sync.Mutex
	var stopErr error
	stop := func() error {
		jobMu.Lock()
		defer jobMu.Unlock()
		if jobClosed {
			return nil
		}
		err := cancelConversion(func() error { return windows.TerminateJobObject(job, 1) }, func() error {
			closeErr := windows.CloseHandle(job)
			if closeErr == nil {
				jobClosed = true
			}
			killErr := cmd.Process.Kill()
			if errors.Is(killErr, os.ErrProcessDone) {
				killErr = nil
			}
			return errors.Join(errJobDrain, closeErr, killErr)
		})
		stopErr = errors.Join(stopErr, err)
		return err
	}
	cmd.Cancel = func() error { <-ready; return stop() }
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
			_ = stop()
		}
		if !readyClosed {
			close(ready)
		}
		if !waited {
			err = errors.Join(err, cmd.Wait())
		}
		err = errors.Join(err, stopErr)
		if assigned {
			if jobClosed {
				err = errors.Join(err, errJobDrain)
			} else {
				drainCtx, cancel := context.WithTimeout(context.Background(), gitTimeout)
				drainErr := waitJobDrain(drainCtx, func() (uint32, error) { return activeJobProcesses(job) })
				cancel()
				if drainErr != nil {
					err = errors.Join(err, errJobDrain, drainErr)
				}
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
	if err := ctx.Err(); err != nil {
		return err
	}
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
	if err := ctx.Err(); err != nil {
		return err
	}
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

// Job completion notifications may be lost, so accounting decides when the view can be removed.
func activeJobProcesses(job windows.Handle) (uint32, error) {
	var accounting struct {
		user, kernel, periodUser, periodKernel int64
		faults, total, active, terminated      uint32
	}
	var pin runtime.Pinner
	pin.Pin(&accounting)
	err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&accounting)), uint32(unsafe.Sizeof(accounting)), nil)
	pin.Unpin()
	return accounting.active, err
}

func waitJobDrain(ctx context.Context, active func() (uint32, error)) error {
	for {
		count, err := active()
		if err != nil {
			return err
		}
		if count == 0 {
			return nil
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func cancelConversion(terminate, fallback func() error) error {
	if err := terminate(); err != nil {
		return errors.Join(err, fallback())
	}
	return nil
}
