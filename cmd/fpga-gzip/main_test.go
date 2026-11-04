package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lfhy/fpga-gzip/pipeline"
)

func TestValidateRunResultRejectsZeroByteSuccess(t *testing.T) {
	inputPath := filepath.Join(t.TempDir(), "input.bin")
	if err := os.WriteFile(inputPath, []byte("some input"), 0600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		result pipeline.Result
		wantOK bool
	}{
		{
			name: "valid",
			result: pipeline.Result{
				InputBytes:  10,
				OutputBytes: 20,
			},
			wantOK: true,
		},
		{
			name:   "zero result",
			result: pipeline.Result{},
		},
		{
			name: "wrong input size",
			result: pipeline.Result{
				InputBytes:  9,
				OutputBytes: 20,
			},
		},
		{
			name: "empty output",
			result: pipeline.Result{
				InputBytes: 10,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateRunResult(inputPath, test.result)
			if test.wantOK && err != nil {
				t.Fatalf("validateRunResult() error = %v", err)
			}
			if !test.wantOK && err == nil {
				t.Fatal("validateRunResult() accepted an incomplete result")
			}
		})
	}
}
