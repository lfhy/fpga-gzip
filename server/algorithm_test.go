package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lfhy/fpga-gzip/firmware"
	"github.com/lfhy/fpga-gzip/pipeline"
)

type imageOnly struct{ pipeline.Hardware }

func (*imageOnly) FirmwareInfo(context.Context) (firmware.Info, error) {
	return firmware.Info{Magic: firmware.Magic, ABI: firmware.ABI, Algorithm: firmware.LZ4, Format: 1, Cores: 4, Features: 3, Build: 42}, nil
}
func (*imageOnly) Close() error { return nil }

func TestServerRejectsWrongImageWithoutHardwareOperations(t *testing.T) {
	s, err := New(Config{HardwareFactory: func(context.Context) (pipeline.Hardware, error) { return &imageOnly{}, nil }}, WithAlgorithms(firmware.Gzip, firmware.LZ4, firmware.MinLZ))
	if err != nil {
		t.Fatal(err)
	}
	for _, algorithm := range []string{"gzip", "minlz"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/compress?algorithm="+algorithm, strings.NewReader("abc")))
		if w.Code != http.StatusConflict {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
