// Package firmware describes the versioned compression shell shared by FPGA
// images. Algorithm IDs identify the bytes on C2H, independently of the SDK's
// historical gzip API and of the number of compression engines in an image.
package firmware

import (
	"context"
	"errors"
	"fmt"
)

type Algorithm uint32

const (
	Raw Algorithm = 1 + iota
	Gzip
	LZ4
	Zstd
	MinLZ
)

func (a Algorithm) String() string {
	switch a {
	case Raw:
		return "raw"
	case Gzip:
		return "gzip"
	case LZ4:
		return "lz4"
	case Zstd:
		return "zstd"
	case MinLZ:
		return "minlz"
	default:
		return fmt.Sprintf("unknown(%d)", uint32(a))
	}
}

func ParseAlgorithm(name string) (Algorithm, error) {
	switch name {
	case "raw", "none":
		return Raw, nil
	case "gzip", "fpga", "fpga-gzip":
		return Gzip, nil
	case "lz4", "fpga-lz4":
		return LZ4, nil
	case "zstd", "fpga-zstd":
		return Zstd, nil
	case "minlz", "fpga-minlz":
		return MinLZ, nil
	default:
		return 0, fmt.Errorf("unsupported FPGA algorithm %q", name)
	}
}

const (
	RegMagic             uint32 = 0x44
	RegABI               uint32 = 0x48
	RegAlgorithm         uint32 = 0x4c
	RegFormat            uint32 = 0x50
	RegCores             uint32 = 0x54
	RegFeatures          uint32 = 0x58
	RegBuildLo           uint32 = 0x5c
	RegBuildHi           uint32 = 0x60
	Magic                uint32 = 0x434d5058 // CMPX
	ABI                  uint32 = 0x00010000 // major 1, minor 0
	FormatV1             uint32 = 1
	FeatureEOP           uint32 = 1 << 0
	FeatureStandardFrame uint32 = 1 << 1
	LegacyEOP            uint32 = 0x475a0001
)

var (
	ErrIncompatible = errors.New("incompatible FPGA firmware")
	ErrAlgorithm    = errors.New("FPGA firmware algorithm mismatch")
)

// Info is read-only and repeated in each independent 0x1000-byte control bank.
// Legacy is true only for the previously validated gzip EOP shell. An
// unrecognized/uninitialized BAR is never inferred to be a compression image.
type Info struct {
	Magic     uint32    `json:"magic"`
	ABI       uint32    `json:"abi"`
	Algorithm Algorithm `json:"algorithm_id"`
	Format    uint32    `json:"format"`
	Cores     uint32    `json:"cores"`
	Features  uint32    `json:"features"`
	Build     uint64    `json:"build"`
	Legacy    bool      `json:"legacy"`
}

type RegisterReader interface {
	ReadReg32(context.Context, uint32) (uint32, error)
}
type Provider interface {
	FirmwareInfo(context.Context) (Info, error)
}

func Inspect(ctx context.Context, r RegisterReader) (Info, error) {
	magic, err := r.ReadReg32(ctx, RegMagic)
	if err != nil {
		return Info{}, fmt.Errorf("read firmware identity: %w", err)
	}
	if magic == 0 {
		cap, err := r.ReadReg32(ctx, 0x40)
		if err != nil {
			return Info{}, err
		}
		if cap == LegacyEOP {
			return Info{Algorithm: Gzip, Format: FormatV1, Features: FeatureEOP | FeatureStandardFrame, Legacy: true}, nil
		}
	}
	if magic != Magic {
		return Info{}, fmt.Errorf("%w: shell magic %#x", ErrIncompatible, magic)
	}
	i := Info{Magic: magic}
	values := []*uint32{&i.ABI, nil, &i.Format, &i.Cores, &i.Features}
	for n, dst := range values {
		v, err := r.ReadReg32(ctx, RegABI+uint32(n)*4)
		if err != nil {
			return Info{}, err
		}
		if dst == nil {
			i.Algorithm = Algorithm(v)
		} else {
			*dst = v
		}
	}
	lo, err := r.ReadReg32(ctx, RegBuildLo)
	if err != nil {
		return Info{}, err
	}
	hi, err := r.ReadReg32(ctx, RegBuildHi)
	if err != nil {
		return Info{}, err
	}
	i.Build = uint64(hi)<<32 | uint64(lo)
	if err := i.Validate(); err != nil {
		return Info{}, err
	}
	return i, nil
}

func (i Info) Validate() error {
	if i.Legacy {
		if i.Algorithm == Gzip && i.Format == FormatV1 && i.Features&FeatureEOP != 0 {
			return nil
		}
		return fmt.Errorf("%w: invalid legacy identity", ErrIncompatible)
	}
	if i.Magic != Magic || i.ABI>>16 != ABI>>16 || i.Format != FormatV1 || i.Cores < 1 || i.Cores > 4 || i.Features&FeatureEOP == 0 || i.Algorithm < Raw || i.Algorithm > MinLZ {
		return fmt.Errorf("%w: %+v", ErrIncompatible, i)
	}
	if i.Algorithm != Raw && i.Features&FeatureStandardFrame == 0 {
		return fmt.Errorf("%w: standard frame feature missing", ErrIncompatible)
	}
	return nil
}

// Require runs before RESET, START and DMA. Non-device gzip simulators without
// register access retain the original Hardware interface. Every other algorithm
// requires an explicit, versioned identity; legacy firmware is gzip-only.
func Require(ctx context.Context, hardware any, want Algorithm) (Info, error) {
	var i Info
	var err error
	switch h := hardware.(type) {
	case Provider:
		i, err = h.FirmwareInfo(ctx)
	case RegisterReader:
		i, err = Inspect(ctx, h)
	default:
		if want == Gzip {
			return Info{Algorithm: Gzip, Format: FormatV1, Legacy: true}, nil
		}
		return Info{}, fmt.Errorf("%w: hardware cannot report identity", ErrIncompatible)
	}
	if err != nil {
		return Info{}, err
	}
	if err := i.Validate(); err != nil {
		return Info{}, err
	}
	if i.Algorithm != want {
		return Info{}, fmt.Errorf("%w: loaded=%s requested=%s build=%016x", ErrAlgorithm, i.Algorithm, want, i.Build)
	}
	return i, nil
}
