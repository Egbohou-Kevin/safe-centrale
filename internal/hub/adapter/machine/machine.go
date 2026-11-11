// Package machine reads what the hub's board says of itself. It uses the standard
// library only.
package machine

import (
	"io"
	"os"
)

// serialPath is where Linux shows the serial number of a device tree board, a
// Raspberry Pi among them. Not /proc/cpuinfo: its Serial line is a Raspberry Pi kernel
// addition.
const serialPath = "/sys/firmware/devicetree/base/serial-number"

// maxRead bounds what is read of the file, far over any serial.
const maxRead = 256

// Serial is what the board says as its serial number, as it says it, "" where it says
// nothing: outside Linux or on a machine without a device tree.
func Serial() string { return readSerial(serialPath) }

func readSerial(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxRead))
	if err != nil {
		return ""
	}
	return string(b)
}
