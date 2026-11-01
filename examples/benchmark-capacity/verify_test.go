package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"runtime"
	"testing"
)

func encoded(t *testing.T, raw []byte) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &b
}

func verificationFixture(t *testing.T) ([][]byte, [][]*bytes.Buffer) {
	t.Helper()
	inputs := make([][]byte, 2)
	outputs := make([][]*bytes.Buffer, 2)
	for c := range inputs {
		inputs[c] = bytes.Repeat([]byte("complete decoded content verification\n"), 1024)
		for i := 0; i < 5; i++ {
			binary.LittleEndian.PutUint64(inputs[c][:8], uint64(c)+uint64(i)*256+1000000)
			outputs[c] = append(outputs[c], encoded(t, inputs[c]))
		}
	}
	return inputs, outputs
}

func TestParallelVerificationChecksAllMembersAndRestoresProcs(t *testing.T) {
	inputs, outputs := verificationFixture(t)
	original := append([]byte(nil), inputs[0]...)
	previous := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(previous)
	if err := verifyOutputs(inputs, outputs, 1, len(inputs[0]), 4); err != nil {
		t.Fatal(err)
	}
	if runtime.GOMAXPROCS(0) != 2 || !bytes.Equal(inputs[0], original) {
		t.Fatal("verification changed compression P count or shared input")
	}
}

func TestParallelVerificationRejectsValidGzipWithWrongContent(t *testing.T) {
	for _, fault := range []string{"core", "body", "trailer", "short"} {
		t.Run(fault, func(t *testing.T) {
			inputs, outputs := verificationFixture(t)
			switch fault {
			case "core":
				outputs[1][2] = outputs[0][2]
			case "body", "short":
				raw := append([]byte(nil), inputs[1]...)
				binary.LittleEndian.PutUint64(raw[:8], 1+2*256+1000000)
				if fault == "body" {
					raw[9000] ^= 1
				} else {
					raw = raw[:len(raw)-1]
				}
				outputs[1][2] = encoded(t, raw)
			case "trailer":
				b := outputs[1][2].Bytes()
				outputs[1][2] = bytes.NewBuffer(b[:len(b)-4])
			}
			previous := runtime.GOMAXPROCS(0)
			if err := verifyOutputs(inputs, outputs, 1, len(inputs[0]), 4); err == nil {
				t.Fatal("accepted mismatched or incomplete output")
			}
			if runtime.GOMAXPROCS(0) != previous {
				t.Fatal("verification failed to restore compression P count")
			}
		})
	}
}
