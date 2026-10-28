package config

import (
	"testing"
)

func TestParsePositionalAndSizes(t *testing.T) {
	cfg, err := Parse([]string{
		"--progress=false",
		"--ring-slots=4",
		"--h2c-slot-bytes=2MiB",
		"--c2h-request-bytes=64KiB",
		"--max-procs=2",
		"in.bin",
		"out.gz",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InputPath != "in.bin" || cfg.OutputPath != "out.gz" {
		t.Fatalf("paths = %q, %q", cfg.InputPath, cfg.OutputPath)
	}
	if cfg.RingSlots != 4 || cfg.H2CSlotBytes != 2*1024*1024 || cfg.C2HRequestBytes != 64*1024 {
		t.Fatalf("unexpected pipeline config: %+v", cfg)
	}
	if cfg.Progress {
		t.Fatal("progress should be false")
	}
	if cfg.MaxProcs != 2 {
		t.Fatalf("max-procs = %d, want 2", cfg.MaxProcs)
	}
}
