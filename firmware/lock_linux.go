//go:build linux

package firmware

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

var ErrSwitching = errors.New("FPGA firmware switch is in progress")

// CardLock serializes programming against all cooperating DMA handles of the
// current Unix user. An exclusive gate first stops new admissions, then the
// activity lock waits for existing transactions to close. SIGKILL releases both.
type CardLock struct{ gate, activity *os.File }

func lockFiles(device string) (*CardLock, error) {
	path, err := filepath.Abs(device)
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(path)))
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("fpga-compress-locks-%d", os.Getuid()))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	gate, err := os.OpenFile(filepath.Join(dir, key+".gate"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	activity, err := os.OpenFile(filepath.Join(dir, key+".active"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		gate.Close()
		return nil, err
	}
	return &CardLock{gate: gate, activity: activity}, nil
}

// LockTransaction is nonblocking so callers can apply backpressure or use CPU
// fallback while an image is being loaded. Keep the returned lock until all
// stream syscalls have finished and the BAR has closed.
func LockTransaction(device string) (*CardLock, error) {
	l, err := lockFiles(device)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(l.gate.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err == nil {
		err = syscall.Flock(int(l.activity.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	}
	if err != nil {
		l.Close()
		return nil, fmt.Errorf("%w: %v", ErrSwitching, err)
	}
	_ = l.gate.Close()
	l.gate = nil
	return l, nil
}

// LockProgramming blocks new DMA opens, then drains existing handles. It does
// not stop processes or unload drivers; the caller owns the programming steps.
func LockProgramming(ctx context.Context, device string) (*CardLock, error) {
	l, err := lockFiles(device)
	if err != nil {
		return nil, err
	}
	for _, file := range []*os.File{l.gate, l.activity} {
		for {
			if err := ctx.Err(); err != nil {
				l.Close()
				return nil, err
			}
			err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				break
			}
			if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
				l.Close()
				return nil, err
			}
			timer := time.NewTimer(10 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				l.Close()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return l, nil
}

func (l *CardLock) Close() error {
	if l == nil {
		return nil
	}
	var err error
	if l.activity != nil {
		err = l.activity.Close()
		l.activity = nil
	}
	if l.gate != nil {
		e := l.gate.Close()
		if err == nil {
			err = e
		}
		l.gate = nil
	}
	return err
}
