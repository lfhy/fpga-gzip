//go:build !linux

package main

func processCPUSeconds() (float64, bool) { return 0, false }
