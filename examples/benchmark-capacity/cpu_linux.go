//go:build linux

package main

import "syscall"

// Includes this process's userspace and syscall CPU time. IRQ/workqueue and
// other processes' CPU time are excluded, as is post-timing gzip verification.
func processCPUSeconds() (float64, bool) {
	var r syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &r); err != nil {
		return 0, false
	}
	return float64(r.Utime.Sec+r.Stime.Sec) + float64(r.Utime.Usec+r.Stime.Usec)/1e6, true
}
