//go:build linux

package firmware

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestProgrammingClosesAdmissionAndWaitsForDMA(t *testing.T) {
	device := filepath.Join(t.TempDir(), "user")
	transaction, err := LockTransaction(device)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ready := make(chan *CardLock, 1)
	errorsCh := make(chan error, 1)
	go func() {
		l, err := LockProgramming(ctx, device)
		if err != nil {
			errorsCh <- err
			return
		}
		ready <- l
	}()
	deadline := time.Now().Add(time.Second)
	for {
		l, err := LockTransaction(device)
		if errors.Is(err, ErrSwitching) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		l.Close()
		if time.Now().After(deadline) {
			t.Fatal("programmer did not close admission")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case l := <-ready:
		l.Close()
		t.Fatal("programmed with active DMA")
	default:
	}
	transaction.Close()
	var programming *CardLock
	select {
	case programming = <-ready:
	case err := <-errorsCh:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if l, err := LockTransaction(device); err == nil {
		l.Close()
		programming.Close()
		t.Fatal("new DMA entered while programming")
	}
	programming.Close()
	l, err := LockTransaction(device)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
}
