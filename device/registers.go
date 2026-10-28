package device

// User-BAR register offsets for the gzip shell implemented in the FPGA.
const (
	RegCtrl          uint32 = 0x00
	RegStatus        uint32 = 0x04
	RegInputSizeLo   uint32 = 0x08
	RegInputSizeHi   uint32 = 0x0c
	RegOutBytesLo    uint32 = 0x10
	RegOutBytesHi    uint32 = 0x14
	RegInBytesLo     uint32 = 0x18
	RegInBytesHi     uint32 = 0x1c
	RegC2HRequestLen uint32 = 0x20
	RegDebug         uint32 = 0x24
)

// Control register bits.
const (
	CtrlStart     uint32 = 1 << 0
	CtrlSoftReset uint32 = 1 << 1
	CtrlC2HStart  uint32 = 1 << 2
)

// Status register bits.
const (
	StatusBusy      uint32 = 1 << 0
	StatusDone      uint32 = 1 << 1
	StatusResetBusy uint32 = 1 << 2
	StatusError     uint32 = 1 << 3
	StatusC2HDone   uint32 = 1 << 4
)

func StatusName(status uint32) string {
	switch {
	case status&StatusError != 0:
		return "ERROR"
	case status&StatusDone != 0:
		return "DONE"
	case status&StatusBusy != 0:
		return "RUN"
	default:
		return "IDLE"
	}
}
