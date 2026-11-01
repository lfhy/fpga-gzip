//go:build !linux || !amd64

package device

import "errors"

func (*Device) EnableRegisterMmap() error                        { return errors.New("mapped registers require Linux/amd64") }
func (*Device) readMappedRegister(uint32) (uint32, bool, error)  { return 0, false, nil }
func (*Device) writeMappedRegister(uint32, uint32) (bool, error) { return false, nil }
func (*Device) closeRegisterMap() error                          { return nil }
