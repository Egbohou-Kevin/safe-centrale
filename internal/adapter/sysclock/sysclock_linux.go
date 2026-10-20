package sysclock

import "syscall"

// staUnsync is STA_UNSYNC in the kernel's timex status: the clock is not synchronised.
const staUnsync = 0x0040

// timeError is TIME_ERROR, what adjtimex returns while the clock is not synchronised.
const timeError = 5

func read() (bool, bool) {
	var tx syscall.Timex // Modes 0: read only
	state, err := syscall.Adjtimex(&tx)
	if err != nil {
		return false, false
	}
	return fromTimex(state, int64(tx.Status)), true
}

func fromTimex(state int, status int64) bool {
	return state != timeError && status&staUnsync == 0
}
