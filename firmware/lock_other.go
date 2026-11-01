//go:build !linux

package firmware

import (
	"context"
	"errors"
)

var ErrSwitching = errors.New("FPGA firmware switch is in progress")

type CardLock struct{}

func LockTransaction(string) (*CardLock, error) { return &CardLock{}, nil }
func LockProgramming(context.Context, string) (*CardLock, error) {
	return nil, errors.New("FPGA programming requires Linux")
}
func (*CardLock) Close() error { return nil }
