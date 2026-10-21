package x728

import "testing"

// The gauge's registers convert as in the maker's script: voltage = reg*1.25/1000/16
// volts, charge = reg/256 percent. The SMBus read gives each word low byte first.
func TestTheGaugeConvertsAsTheMakersScript(t *testing.T) {
	for word, want := range map[uint16]int{0x40d4: 4245, 0x00b9: 3700, 0x0096: 3000} {
		if mv := milliVoltsOf(swap(word)); mv != want {
			t.Errorf("voltage %#04x: %d mV, want %d", word, mv, want)
		}
	}
	// 0x3780 is 55.5 %; a gauge above 100 % is full.
	for word, want := range map[uint16]int{0x8037: 55, 0x0064: 100, 0x40d4: 100, 0x0000: 0} {
		if p := percentOf(swap(word)); p != want {
			t.Errorf("charge %#04x: %d %%, want %d", word, p, want)
		}
	}
}

func TestOpenWantsTheWholeModule(t *testing.T) {
	for name, o := range map[string]Options{
		"no chip":    {I2CBus: "/dev/i2c-1", MainsLine: 6, CutLine: 26, CutPulse: 1},
		"same lines": {GPIOChip: "pinctrl-bcm2711", I2CBus: "/dev/i2c-1", MainsLine: 6, CutLine: 6, CutPulse: 1},
		"no pulse":   {GPIOChip: "pinctrl-bcm2711", I2CBus: "/dev/i2c-1", MainsLine: 6, CutLine: 26},
	} {
		if _, err := Open(o); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
}
