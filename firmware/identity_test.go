package firmware

import (
	"context"
	"errors"
	"testing"
)

type registers map[uint32]uint32

func (r registers) ReadReg32(_ context.Context, offset uint32) (uint32, error) { return r[offset], nil }

func goodRegisters(a Algorithm) registers {
	return registers{RegMagic: Magic, RegABI: ABI, RegAlgorithm: uint32(a), RegFormat: 1, RegCores: 4, RegFeatures: 3, RegBuildLo: 42}
}

func TestIdentityRejectsWrongAlgorithmAndUnknownABI(t *testing.T) {
	ctx := context.Background()
	for _, algorithm := range []Algorithm{Raw, Gzip, LZ4, Zstd} {
		r := goodRegisters(algorithm)
		got, err := Require(ctx, r, algorithm)
		if err != nil || got.Algorithm != algorithm || got.Build != 42 {
			t.Fatal(got, err)
		}
		want := Gzip
		if algorithm == Gzip {
			want = Zstd
		}
		if _, err := Require(ctx, r, want); !errors.Is(err, ErrAlgorithm) {
			t.Fatal(err)
		}
	}
	for _, edit := range []func(registers){
		func(r registers) { r[RegMagic] = 0xdeadbeef },
		func(r registers) { r[RegABI] = 0x00020000 },
		func(r registers) { r[RegFormat] = 2 },
		func(r registers) { r[RegCores] = 5 },
		func(r registers) { r[RegFeatures] = 0 },
	} {
		r := goodRegisters(Gzip)
		edit(r)
		if _, err := Inspect(ctx, r); !errors.Is(err, ErrIncompatible) {
			t.Fatal(err)
		}
	}
	if _, err := Inspect(ctx, registers{}); !errors.Is(err, ErrIncompatible) {
		t.Fatal("accepted zero BAR", err)
	}
}

func TestLegacyImageIsGzipOnly(t *testing.T) {
	r := registers{0x40: LegacyEOP}
	i, err := Require(context.Background(), r, Gzip)
	if err != nil || !i.Legacy {
		t.Fatal(i, err)
	}
	for _, a := range []Algorithm{Raw, LZ4, Zstd} {
		if _, err := Require(context.Background(), r, a); !errors.Is(err, ErrAlgorithm) {
			t.Fatal(a, err)
		}
	}
}
