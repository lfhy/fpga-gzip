package pipeline

import (
	"fmt"
	"os"
)

// AppendCSV appends one result row, creating the header when needed.
func AppendCSV(path string, result Result, opts Options) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o666)
	if err != nil {
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		if _, err := fmt.Fprintln(file, "Input_bytes,Output_bytes,Ring_slots,H2C_slot_bytes,C2H_request_bytes,Total_s,Sync_s,Pipeline_s,FPGA_s,C2H_s,Total_MBps,Pipeline_MBps,Compression_ratio"); err != nil {
			return err
		}
	}

	totalSeconds := result.Total.Seconds()
	syncSeconds := result.Sync.Seconds()
	pipelineSeconds := result.Pipeline.Seconds()
	fpgaSeconds := result.FPGA.Seconds()
	c2hSeconds := result.C2H.Seconds()
	totalMBps := 0.0
	pipelineMBps := 0.0
	if totalSeconds > 0 {
		totalMBps = float64(result.InputBytes) / (1024 * 1024) / totalSeconds
	}
	if pipelineSeconds > 0 {
		pipelineMBps = float64(result.InputBytes) / (1024 * 1024) / pipelineSeconds
	}
	ratio := 0.0
	if result.InputBytes > 0 {
		ratio = float64(result.OutputBytes) / float64(result.InputBytes)
	}

	_, err = fmt.Fprintf(file, "%d,%d,%d,%d,%d,%.9f,%.9f,%.9f,%.9f,%.9f,%.6f,%.6f,%.9f\n",
		result.InputBytes, result.OutputBytes, opts.RingSlots,
		opts.H2CSlotBytes, opts.C2HRequestBytes,
		totalSeconds, syncSeconds, pipelineSeconds, fpgaSeconds, c2hSeconds,
		totalMBps, pipelineMBps, ratio)
	if err != nil {
		return err
	}
	return file.Sync()
}
